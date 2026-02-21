package overseer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// HubConfig holds all options for creating a Hub.
type HubConfig struct {
	DB          *sql.DB                  // reserved for future exit-history persistence (job 1370); not used for task CRUD
	EventLog    io.Writer                // optional; nil = no log
	Actions     map[string]ActionHandler // built once at startup; immutable
	Pool        *PoolManager             // manages concurrency + queue
	TrustedNets []*net.IPNet             // passed through for transport if needed
}

// TaskRecord holds the in-memory state for a task.
// Tasks are not persisted — state is lost on restart.
type TaskRecord struct {
	TaskID        string
	Action        string            // action handler name
	Params        map[string]string // action parameters as key-value map
	RetryPolicy   *RetryPolicy
	State         TaskState // see StateActive / StateStopped / StateErrored
	RestartCount  int
	ExitCount     int
	CreatedAt     time.Time
	LastStartedAt *time.Time
	LastExitedAt  *time.Time
	ErrorMessage  string
}

// Task is the in-memory representation of a task.
type Task struct {
	mu          sync.Mutex
	record      TaskRecord
	worker      *Worker     // current running worker, nil if not running
	exitHistory []time.Time // non-intentional exits for threshold tracking
	outputSeq   int64       // per-task monotonic output sequence counter; protected by mu
}

// Hub is the central orchestrator for all tasks, clients, and pool management.
type Hub struct {
	mu            sync.RWMutex
	clients       map[Conn]struct{}              // all connected clients
	subscriptions map[Conn]map[string]struct{}   // conn → set of subscribed task_ids; protected by h.mu
	tasks         map[string]*Task               // task_id → Task
	pending       map[string]struct{}            // task IDs currently being started; prevents TOCTOU duplicate starts; protected by h.mu
	actions       map[string]ActionHandler       // immutable after startup
	pool          *PoolManager                   // manages concurrency + queue
	db            *sql.DB                        // optional; used for exit_history persistence (nil = no persistence)
	metrics       *Metrics                       // in-memory metrics registry; never nil after NewHub
	eventLog      *json.Encoder
	eventLogMu    sync.Mutex // serialises concurrent Encode calls on eventLog
	shutdownCh    chan struct{}
	shutdownOnce  sync.Once
}

// NewHub creates a Hub with an empty in-memory task map.
// Tasks are not persisted across restarts — each startup begins fresh.
// If cfg.DB is non-nil, exit_history records are persisted to SQLite and a
// background pruner goroutine is started to prevent unbounded table growth.
func NewHub(cfg HubConfig) *Hub {
	h := &Hub{
		clients:       make(map[Conn]struct{}),
		subscriptions: make(map[Conn]map[string]struct{}),
		tasks:         make(map[string]*Task),
		pending:       make(map[string]struct{}),
		actions:       cfg.Actions,
		pool:          cfg.Pool,
		db:            cfg.DB,
		metrics:       NewMetrics(),
		shutdownCh:    make(chan struct{}),
	}
	if cfg.EventLog != nil {
		h.eventLog = json.NewEncoder(cfg.EventLog)
	}

	// Start background exit-history pruner when a DB is configured.
	// Prunes records older than 24 hours every hour to prevent table bloat.
	if h.db != nil {
		go func() {
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := pruneExitHistory(h.db, time.Now().Add(-24*time.Hour)); err != nil {
						log.Printf("exit_history: prune failed: %v", err)
					}
				case <-h.shutdownCh:
					return
				}
			}
		}()
	}

	return h
}

// Shutdown signals all pending restarts to abort, stops every running worker,
// and waits for them to exit or ctx to expire.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.shutdownOnce.Do(func() { close(h.shutdownCh) })

	// Collect running workers under locks, then call Stop() with no locks held.
	var workers []*Worker
	h.mu.RLock()
	for _, task := range h.tasks {
		task.mu.Lock()
		if task.worker != nil {
			task.worker.mu.Lock()
			if task.worker.State == WorkerRunning {
				workers = append(workers, task.worker)
			}
			task.worker.mu.Unlock()
		}
		task.mu.Unlock()
	}
	h.mu.RUnlock()

	for _, w := range workers {
		w.Stop()
	}

	// Poll until all workers have exited or context expires.
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if !h.hasRunningWorkers() {
				return nil
			}
		}
	}
}

func (h *Hub) hasRunningWorkers() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, task := range h.tasks {
		task.mu.Lock()
		running := task.worker != nil && task.worker.State == WorkerRunning
		task.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

// parseTrustedCIDRs parses a comma-separated list of bare IPs and CIDR ranges.
// Returns nil, nil for an empty string; callers interpret nil as "allow all".
func parseTrustedCIDRs(s string) ([]*net.IPNet, error) {
	if s == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, cidr := range strings.Split(s, ",") {
		cidr = strings.TrimSpace(cidr)
		if !strings.Contains(cidr, "/") {
			ip := net.ParseIP(cidr)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP in trusted CIDRs: %s", cidr)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			_, ipnet, _ := net.ParseCIDR(fmt.Sprintf("%s/%d", cidr, bits))
			nets = append(nets, ipnet)
		} else {
			_, ipnet, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR in trusted CIDRs: %s", cidr)
			}
			nets = append(nets, ipnet)
		}
	}
	return nets, nil
}

// detectLocalSubnets returns loopback (127.0.0.0/8, ::1/128) plus all
// subnets found on local network interfaces.
func detectLocalSubnets() []*net.IPNet {
	var nets []*net.IPNet
	_, lo4, _ := net.ParseCIDR("127.0.0.0/8")
	_, lo6, _ := net.ParseCIDR("::1/128")
	nets = append(nets, lo4, lo6)

	ifaces, err := net.Interfaces()
	if err != nil {
		return nets
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				nets = append(nets, ipnet)
			}
		}
	}
	return nets
}

func isTrusted(r *http.Request, nets []*net.IPNet) bool {
	if len(nets) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *Hub) LogEvent(v interface{}) {
	if h.eventLog == nil {
		return
	}
	h.eventLogMu.Lock()
	_ = h.eventLog.Encode(v)
	h.eventLogMu.Unlock()
}

// AddClient registers a connection with the hub and initialises its subscription set.
func (h *Hub) AddClient(conn Conn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.subscriptions[conn] = make(map[string]struct{})
	h.mu.Unlock()
}

// RemoveClient deregisters a connection and cleans up its subscriptions.
func (h *Hub) RemoveClient(conn Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	delete(h.subscriptions, conn)
	h.mu.Unlock()
}

// Broadcast serialises msg to JSON and writes it to every connected client.
// Dead connections are removed after the broadcast pass.
func (h *Hub) Broadcast(msg interface{}) {
	h.mu.RLock()
	conns := make([]Conn, 0, len(h.clients))
	for conn := range h.clients {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()

	var dead []Conn
	for _, conn := range conns {
		mu := conn.WriteLock()
		mu.Lock()
		err := conn.WriteJSON(msg)
		mu.Unlock()
		if err != nil {
			log.Printf("broadcast write error: %v", err)
			dead = append(dead, conn)
		}
	}
	for _, c := range dead {
		h.RemoveClient(c)
		c.Close()
	}
}

// BroadcastToSubscribers sends msg only to connections subscribed to taskID.
// Task-specific events (output, exited, restarting, errored, started) are
// routed through this method so unsubscribed clients do not receive them.
func (h *Hub) BroadcastToSubscribers(taskID string, msg interface{}) {
	h.mu.RLock()
	var targets []Conn
	for conn, subs := range h.subscriptions {
		if _, ok := subs[taskID]; ok {
			targets = append(targets, conn)
		}
	}
	h.mu.RUnlock()

	var dead []Conn
	for _, conn := range targets {
		mu := conn.WriteLock()
		mu.Lock()
		err := conn.WriteJSON(msg)
		mu.Unlock()
		if err != nil {
			log.Printf("broadcast write error: %v", err)
			dead = append(dead, conn)
		}
	}
	for _, c := range dead {
		h.RemoveClient(c)
		c.Close()
	}
}

// HandleClient runs the read loop for conn until the connection closes.
func (h *Hub) HandleClient(conn Conn) {
	h.AddClient(conn)
	defer func() {
		h.RemoveClient(conn)
		conn.Close()
	}()

	for {
		var msg IncomingMessage
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}

		switch msg.Type {
		case "start":
			h.handleStart(conn, msg)
		case "list":
			h.handleList(conn, msg)
		case "stop":
			h.handleStop(conn, msg)
		case "reset":
			h.handleReset(conn, msg)
		case "replay":
			h.handleReplay(conn, msg)
		case "describe":
			h.handleDescribe(conn, msg)
		case "pool_info":
			h.handlePoolInfo(conn, msg)
		case "purge":
			h.handlePurge(conn, msg)
		case "set_pool":
			h.handleSetPool(conn, msg)
		case "subscribe":
			h.handleSubscribe(conn, msg)
		case "unsubscribe":
			h.handleUnsubscribe(conn, msg)
		case "metrics":
			h.handleMetrics(conn, msg)
		default:
			h.sendError(conn, msg.ID, "unknown message type: "+msg.Type)
		}
	}
}

// workerCB builds the callbacks injected into each new Worker.
// When task is non-nil, output messages are stamped with a per-task
// monotonically increasing sequence number so reconnecting clients can
// detect gaps caused by ring buffer overflow.
func (h *Hub) workerCB() WorkerCallbacks {
	return h.workerCBForTask(nil)
}

// workerCBForTask builds callbacks bound to a specific task for sequence numbering.
// Output and exit events are routed to BroadcastToSubscribers so only subscribed
// clients receive task-specific events.
func (h *Hub) workerCBForTask(task *Task) WorkerCallbacks {
	return WorkerCallbacks{
		OnOutput: func(msg *OutputMessage) {
			if task != nil {
				task.mu.Lock()
				task.outputSeq++
				msg.Seq = task.outputSeq
				taskAction := task.record.Action
				task.mu.Unlock()
				h.metrics.RecordOutput(msg.TaskID, taskAction)
				h.BroadcastToSubscribers(msg.TaskID, *msg)
			} else {
				h.Broadcast(*msg)
			}
		},
		LogEvent: func(v any) { h.LogEvent(v) },
		OnExited: func(w *Worker, code int, intentional bool, t time.Time) {
			h.onWorkerExited(w, code, intentional, t)
		},
	}
}

// dedupeFingerprint builds a stable, collision-resistant fingerprint from the
// action name and the values of the specified parameter keys. Returns "" when
// dedupeKey is empty, signalling that deduplication is inactive for this action.
// Keys are sorted before joining to ensure stability regardless of map iteration order.
func dedupeFingerprint(action string, params map[string]string, dedupeKey []string) string {
	if len(dedupeKey) == 0 {
		return ""
	}
	keys := make([]string, len(dedupeKey))
	copy(keys, dedupeKey)
	sort.Strings(keys)

	var parts []string
	parts = append(parts, action)
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "\x00")
}

// handleStart processes a "start" request.
func (h *Hub) handleStart(conn Conn, msg IncomingMessage) {
	if h.actions == nil {
		h.sendError(conn, msg.ID, "no actions configured")
		return
	}

	if msg.Action == "" {
		h.sendError(conn, msg.ID, "action is required")
		return
	}

	// Look up handler without holding h.mu while calling handler methods.
	h.mu.RLock()
	handler, ok := h.actions[msg.Action]
	h.mu.RUnlock()
	if !ok {
		h.sendError(conn, msg.ID, fmt.Sprintf("unknown action: %q", msg.Action))
		return
	}

	// Validate params before touching any shared state.
	if err := handler.Validate(msg.Params); err != nil {
		h.sendError(conn, msg.ID, "validation error: "+err.Error())
		return
	}

	action := msg.Action
	params := msg.Params
	if params == nil {
		params = map[string]string{}
	}

	// Dedup check: scan running tasks for a matching fingerprint before reserving.
	// Only active when the action's DedupeKey is non-empty.
	info := handler.Describe()
	if fp := dedupeFingerprint(action, params, info.DedupeKey); fp != "" {
		h.mu.RLock()
		for _, existing := range h.tasks {
			existing.mu.Lock()
			running := existing.worker != nil && existing.worker.State == WorkerRunning
			existingFP := dedupeFingerprint(existing.record.Action, existing.record.Params, info.DedupeKey)
			existingTaskID := existing.record.TaskID
			existing.mu.Unlock()
			if running && existingFP == fp {
				h.mu.RUnlock()
				h.sendJSON(conn, ErrorMessage{
					Type:           "error",
					ID:             msg.ID,
					Message:        fmt.Sprintf("duplicate task: already running as %s", existingTaskID),
					ExistingTaskID: existingTaskID,
				})
				return
			}
		}
		h.mu.RUnlock()
	}

	taskID := msg.TaskID
	if taskID == "" {
		taskID = newUUID()
	}

	// Atomically check for duplicates (running or currently starting) and reserve
	// the taskID in h.pending. This closes the TOCTOU race where two concurrent
	// start requests for the same taskID could both pass the duplicate check before
	// either one registers the task in h.tasks.
	h.mu.Lock()
	_, existsInTasks := h.tasks[taskID]
	_, existsInPending := h.pending[taskID]
	if existsInTasks || existsInPending {
		h.mu.Unlock()
		h.sendError(conn, msg.ID, "task already running or starting")
		return
	}
	// Reserve the taskID to prevent concurrent start attempts.
	h.pending[taskID] = struct{}{}
	h.mu.Unlock()

	retryPolicy := msg.RetryPolicy

	// startFn is called synchronously by pool.Acquire when a slot is available.
	// pm.mu is NOT held when startFn is called (fixed in job 1360/1365 Acquire refactor),
	// so acquiring h.mu here does not create a pm.mu → h.mu lock-order inversion.
	startFn := func() error {
		now := time.Now().UTC()
		rec := TaskRecord{
			TaskID:        taskID,
			Action:        action,
			Params:        params,
			RetryPolicy:   retryPolicy,
			State:         StateActive,
			CreatedAt:     now,
			LastStartedAt: &now,
		}

		// Load recent exit history from DB for retry-window tracking.
		// This ensures the ErrorWindow check works correctly across overseer restarts.
		task := &Task{record: rec}
		if h.db != nil && retryPolicy != nil && retryPolicy.ErrorWindow > 0 {
			since := now.Add(-retryPolicy.ErrorWindow)
			history, err := loadExitHistory(h.db, action, since)
			if err != nil {
				log.Printf("exit_history: load failed action=%s: %v", action, err)
			} else if len(history) > 0 {
				task.exitHistory = history
				task.record.ExitCount = len(history)
			}
		}

		// Atomically transition from pending → active: remove from pending,
		// register in tasks. This must be a single h.mu.Lock so the task is
		// never briefly absent from both maps.
		h.mu.Lock()
		delete(h.pending, taskID)
		h.tasks[taskID] = task
		h.mu.Unlock()

		w, err := handler.Start(taskID, params, h.workerCBForTask(task))
		if err != nil {
			// Start failed — remove from tasks and clear any pending residue.
			h.mu.Lock()
			delete(h.tasks, taskID)
			delete(h.pending, taskID)
			h.mu.Unlock()
			return err
		}

		task.mu.Lock()
		task.worker = w
		task.mu.Unlock()

		// Auto-subscribe the submitting connection to this task's events.
		h.mu.Lock()
		if subs, ok := h.subscriptions[conn]; ok {
			subs[taskID] = struct{}{}
		}
		h.mu.Unlock()

		h.metrics.RecordStart(taskID, action)

		log.Printf("started worker task=%s pid=%d action=%s", taskID, w.PID, action)
		started := StartedMessage{Type: "started", ID: msg.ID, TaskID: taskID, PID: w.PID, TS: w.StartedAt}
		h.BroadcastToSubscribers(taskID, started)
		h.LogEvent(started)
		return nil
	}

	// cancelFn is called if the task is dequeued, displaced, or expired before starting.
	// Must remove from h.pending to allow the taskID to be reused.
	cancelFn := func(reason error) {
		h.mu.Lock()
		delete(h.pending, taskID)
		h.mu.Unlock()
		h.metrics.RecordDequeued()
		log.Printf("queued task=%s cancelled: %v", taskID, reason)
		h.Broadcast(DequeuedMessage{
			Type:   "dequeued",
			TaskID: taskID,
			Reason: reason.Error(),
			TS:     time.Now().UTC(),
		})
	}

	// stopFn is stored by the pool for excess-task handling.
	stopFn := func() { h.stopWorker(taskID, true) }

	if h.pool != nil {
		result, err := h.pool.Acquire(taskID, action, msg.Force, stopFn, startFn, cancelFn)
		if err != nil {
			h.sendError(conn, msg.ID, "pool rejected task: "+err.Error())
			return
		}
		if result == AcquireQueued {
			h.metrics.RecordEnqueued()
			poolInfo := h.pool.Info(action)
			h.Broadcast(QueuedMessage{
				Type:     "queued",
				ID:       msg.ID,
				TaskID:   taskID,
				Action:   action,
				Position: poolInfo.QueueDepth,
				TS:       time.Now().UTC(),
			})
		}
		// If AcquireRunning, startFn already ran and broadcast "started".
	} else {
		// No pool manager — start immediately.
		if err := startFn(); err != nil {
			h.sendError(conn, msg.ID, err.Error())
		}
	}
}

func (h *Hub) handleList(conn Conn, msg IncomingMessage) {
	var since *time.Time
	if msg.Since != "" {
		t, err := time.Parse(time.RFC3339, msg.Since)
		if err != nil {
			h.sendError(conn, msg.ID, "invalid since timestamp")
			return
		}
		since = &t
	}

	// Collect queued tasks from pool if available.
	var queuedItems []QueuedItemInfo
	if h.pool != nil {
		poolInfo := h.pool.Info("")
		queuedItems = poolInfo.QueueItems
	}

	h.mu.RLock()
	var infos []TaskInfo
	for _, task := range h.tasks {
		task.mu.Lock()
		info := taskInfo(task)
		task.mu.Unlock()
		if since != nil {
			lastAt := info.LastStartedAt
			if info.LastExitedAt != nil && (lastAt == nil || info.LastExitedAt.After(*lastAt)) {
				lastAt = info.LastExitedAt
			}
			if lastAt == nil || lastAt.Before(*since) {
				continue
			}
		}
		infos = append(infos, info)
	}
	h.mu.RUnlock()

	// Add queued tasks not already in the in-memory map.
	existingIDs := make(map[string]bool, len(infos))
	for _, info := range infos {
		existingIDs[info.TaskID] = true
	}
	for _, qi := range queuedItems {
		if existingIDs[qi.TaskID] {
			continue
		}
		qAt := qi.QueuedAt
		infos = append(infos, TaskInfo{
			TaskID:   qi.TaskID,
			Action:   qi.Action,
			State:    StateQueued,
			QueuedAt: &qAt,
		})
	}

	if infos == nil {
		infos = []TaskInfo{}
	}
	h.sendJSON(conn, TasksMessage{Type: "tasks", ID: msg.ID, Tasks: infos})
}

func (h *Hub) handleStop(conn Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendError(conn, msg.ID, "task_id is required")
		return
	}

	// Check if it's a queued task first.
	if h.pool != nil {
		if found := h.pool.Dequeue(msg.TaskID); found {
			h.Broadcast(DequeuedMessage{
				Type:   "dequeued",
				TaskID: msg.TaskID,
				Reason: "stopped",
				TS:     time.Now().UTC(),
			})
			return
		}
	}

	h.mu.RLock()
	task, ok := h.tasks[msg.TaskID]
	h.mu.RUnlock()
	if !ok {
		h.sendError(conn, msg.ID, "task not found")
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	task.record.State = StateStopped

	if task.worker != nil {
		task.worker.mu.Lock()
		running := task.worker.State == WorkerRunning
		task.worker.mu.Unlock()
		if running {
			log.Printf("stopping worker task=%s pid=%d", msg.TaskID, task.worker.PID)
			task.worker.Stop()
		}
	}
}

func (h *Hub) handleReset(conn Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendError(conn, msg.ID, "task_id is required")
		return
	}
	h.mu.RLock()
	task, ok := h.tasks[msg.TaskID]
	h.mu.RUnlock()
	if !ok {
		h.sendError(conn, msg.ID, "task not found")
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	if task.record.State != StateErrored {
		h.sendError(conn, msg.ID, "task is not in errored state")
		return
	}

	task.record.State = StateActive
	task.record.ExitCount = 0
	task.exitHistory = nil

	var w *Worker
	var err error

	if h.actions != nil {
		// Use action handler for reset — bypasses pool (retries are exempt).
		handler, ok := h.actions[task.record.Action]
		if !ok {
			h.sendError(conn, msg.ID, fmt.Sprintf("unknown action: %q", task.record.Action))
			return
		}
		w, err = handler.Start(task.record.TaskID, task.record.Params, h.workerCBForTask(task))
	} else {
		h.sendError(conn, msg.ID, "no actions configured")
		return
	}

	if err != nil {
		h.sendError(conn, msg.ID, err.Error())
		return
	}
	task.worker = w

	now := time.Now().UTC()
	task.record.LastStartedAt = &now

	log.Printf("reset task=%s, started pid=%d", msg.TaskID, w.PID)
	started := StartedMessage{Type: "started", ID: msg.ID, TaskID: msg.TaskID, PID: w.PID, TS: w.StartedAt}
	h.sendJSON(conn, started)
	h.LogEvent(started)
}

func (h *Hub) handleReplay(conn Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendError(conn, msg.ID, "task_id is required")
		return
	}
	h.mu.RLock()
	task, ok := h.tasks[msg.TaskID]
	h.mu.RUnlock()
	if !ok {
		h.sendError(conn, msg.ID, "task not found")
		return
	}

	task.mu.Lock()
	w := task.worker
	task.mu.Unlock()

	if w == nil {
		h.sendError(conn, msg.ID, "no worker for task")
		return
	}

	var since *time.Time
	if msg.Since != "" {
		t, err := time.Parse(time.RFC3339, msg.Since)
		if err != nil {
			h.sendError(conn, msg.ID, "invalid since timestamp")
			return
		}
		since = &t
	}

	events := w.getEvents(since)
	for _, evt := range events {
		switch evt.Type {
		case "output":
			h.sendJSON(conn, OutputMessage{Type: "output", TaskID: evt.TaskID, PID: evt.PID, Stream: evt.Stream, Data: evt.Data, TS: evt.TS, Seq: evt.Seq})
		case "exited":
			ec := 0
			if evt.ExitCode != nil {
				ec = *evt.ExitCode
			}
			h.sendJSON(conn, ExitedMessage{Type: "exited", TaskID: evt.TaskID, PID: evt.PID, ExitCode: ec, Intentional: evt.Intentional, TS: evt.TS})
		}
	}
}

// handleMetrics responds with metrics at global, per-action, or per-task granularity.
// If msg.TaskID is set, returns per-task metrics.
// If msg.Action is set, returns per-action metrics.
// Otherwise returns global metrics.
func (h *Hub) handleMetrics(conn Conn, msg IncomingMessage) {
	resp := MetricsMessage{Type: "metrics", ID: msg.ID}
	switch {
	case msg.TaskID != "":
		resp.Task = h.metrics.TaskSnapshot(msg.TaskID)
	case msg.Action != "":
		resp.Action = h.metrics.ActionSnapshot(msg.Action)
	default:
		g := h.metrics.GlobalSnapshot()
		resp.Global = &g
	}
	h.sendJSON(conn, resp)
}

// handleSubscribe subscribes conn to task-specific events for msg.TaskID.
func (h *Hub) handleSubscribe(conn Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendError(conn, msg.ID, "task_id required for subscribe")
		return
	}
	h.mu.Lock()
	if subs, ok := h.subscriptions[conn]; ok {
		subs[msg.TaskID] = struct{}{}
	}
	h.mu.Unlock()
	h.sendJSON(conn, SubscribedMessage{Type: "subscribed", ID: msg.ID, TaskID: msg.TaskID})
}

// handleUnsubscribe removes conn's subscription to task-specific events for msg.TaskID.
func (h *Hub) handleUnsubscribe(conn Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendError(conn, msg.ID, "task_id required for unsubscribe")
		return
	}
	h.mu.Lock()
	if subs, ok := h.subscriptions[conn]; ok {
		delete(subs, msg.TaskID)
	}
	h.mu.Unlock()
	h.sendJSON(conn, UnsubscribedMessage{Type: "unsubscribed", ID: msg.ID, TaskID: msg.TaskID})
}

// handleDescribe responds with ActionInfo for one or all registered actions.
func (h *Hub) handleDescribe(conn Conn, msg IncomingMessage) {
	if h.actions == nil {
		h.sendError(conn, msg.ID, "no actions configured")
		return
	}

	var actionInfos []ActionInfo

	if msg.Action != "" {
		// Single action lookup.
		h.mu.RLock()
		handler, ok := h.actions[msg.Action]
		h.mu.RUnlock()
		if !ok {
			h.sendError(conn, msg.ID, fmt.Sprintf("unknown action: %q", msg.Action))
			return
		}
		info := handler.Describe()
		info.Name = msg.Action
		actionInfos = []ActionInfo{info}
	} else {
		// All actions — snapshot the map before calling Describe() outside the lock.
		h.mu.RLock()
		type namedHandler struct {
			name    string
			handler ActionHandler
		}
		handlers := make([]namedHandler, 0, len(h.actions))
		for name, handler := range h.actions {
			handlers = append(handlers, namedHandler{name: name, handler: handler})
		}
		h.mu.RUnlock()

		for _, nh := range handlers {
			info := nh.handler.Describe()
			info.Name = nh.name
			actionInfos = append(actionInfos, info)
		}
	}

	if actionInfos == nil {
		actionInfos = []ActionInfo{}
	}
	h.sendJSON(conn, ActionsMessage{Type: "actions", ID: msg.ID, Actions: actionInfos})
}

// handlePoolInfo responds with pool state for a specific action or globally.
func (h *Hub) handlePoolInfo(conn Conn, msg IncomingMessage) {
	if h.pool == nil {
		h.sendJSON(conn, PoolMessage{Type: "pool_info", ID: msg.ID, Action: msg.Action, Pool: PoolInfo{}})
		return
	}
	info := h.pool.Info(msg.Action)
	h.sendJSON(conn, PoolMessage{Type: "pool_info", ID: msg.ID, Action: msg.Action, Pool: info})
}

// handlePurge removes all queued items for a specific action (or all queues).
func (h *Hub) handlePurge(conn Conn, msg IncomingMessage) {
	if h.pool == nil {
		h.sendJSON(conn, PurgedMessage{Type: "purged", ID: msg.ID, Action: msg.Action, Count: 0})
		return
	}
	count, taskIDs := h.pool.PurgeQueue(msg.Action)
	now := time.Now().UTC()
	for _, tid := range taskIDs {
		h.Broadcast(DequeuedMessage{
			Type:   "dequeued",
			TaskID: tid,
			Reason: "purged",
			TS:     now,
		})
	}
	h.sendJSON(conn, PurgedMessage{Type: "purged", ID: msg.ID, Action: msg.Action, Count: count})
}

// handleSetPool dynamically updates pool limits for a specific action or globally.
func (h *Hub) handleSetPool(conn Conn, msg IncomingMessage) {
	if h.pool == nil {
		h.sendError(conn, msg.ID, "no pool configured")
		return
	}
	if err := h.pool.SetLimits(msg.Action, msg.Limit, msg.QueueSize, msg.Excess); err != nil {
		h.sendError(conn, msg.ID, "set_pool error: "+err.Error())
		return
	}
	info := h.pool.Info(msg.Action)
	h.Broadcast(PoolUpdatedMessage{Type: "pool_updated", Action: msg.Action, Pool: info})
	h.sendJSON(conn, PoolMessage{Type: "pool_info", ID: msg.ID, Action: msg.Action, Pool: info})
}

// onWorkerExited is called by the worker callbacks when the process exits.
func (h *Hub) onWorkerExited(w *Worker, exitCode int, intentional bool, now time.Time) {
	exited := ExitedMessage{Type: "exited", TaskID: w.TaskID, PID: w.PID, ExitCode: exitCode, Intentional: intentional, TS: now}
	h.BroadcastToSubscribers(w.TaskID, exited)
	h.LogEvent(exited)

	// Release the pool slot so the next queued task can start.
	if h.pool != nil {
		h.pool.Release(w.TaskID)
	}

	h.mu.RLock()
	task, ok := h.tasks[w.TaskID]
	h.mu.RUnlock()
	if !ok {
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	// Record exit metrics while holding task.mu.
	{
		var runtimeMs int64
		if task.record.LastStartedAt != nil {
			runtimeMs = now.Sub(*task.record.LastStartedAt).Milliseconds()
		}
		h.metrics.RecordExit(w.TaskID, task.record.Action, exitCode, false, runtimeMs)
	}

	task.record.LastExitedAt = &now

	if intentional || task.record.State == StateStopped {
		return
	}

	// Persist non-intentional exit for cross-restart retry-window tracking.
	if h.db != nil {
		if err := recordExit(h.db, task.record.TaskID, task.record.Action, now); err != nil {
			log.Printf("exit_history: record failed task=%s: %v", task.record.TaskID, err)
		}
	}

	rp := task.record.RetryPolicy
	if rp == nil {
		return
	}

	if rp.ErrorWindow != 0 {
		cutoff := now.Add(-rp.ErrorWindow)
		var pruned []time.Time
		for _, t := range task.exitHistory {
			if !t.Before(cutoff) {
				pruned = append(pruned, t)
			}
		}
		task.exitHistory = pruned
	}
	task.exitHistory = append(task.exitHistory, now)
	task.record.ExitCount = len(task.exitHistory)

	if rp.ErrorThreshold > 0 && task.record.ExitCount >= rp.ErrorThreshold {
		task.record.State = StateErrored
		// Overwrite the RecordExit above with errored=true.
		h.metrics.RecordExit(w.TaskID, task.record.Action, exitCode, true, 0)
		h.BroadcastToSubscribers(w.TaskID, ErroredMessage{Type: "errored", TaskID: w.TaskID, PID: w.PID, ExitCount: task.record.ExitCount, TS: now})
		log.Printf("task=%s errored after %d exits", w.TaskID, task.record.ExitCount)
		return
	}

	restartDelay := rp.RestartDelay
	attempt := task.record.RestartCount + 1
	h.metrics.RecordRestart(w.TaskID, task.record.Action)
	h.BroadcastToSubscribers(w.TaskID, RestartingMessage{
		Type:         "restarting",
		TaskID:       w.TaskID,
		PID:          w.PID,
		RestartDelay: restartDelay,
		Attempt:      attempt,
		TS:           now,
	})
	log.Printf("task=%s scheduling restart attempt=%d delay=%s", w.TaskID, attempt, restartDelay)
	go h.doRestart(task, w.PID, attempt, restartDelay)
}

func (h *Hub) doRestart(task *Task, oldPID int, attempt int, delay time.Duration) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-h.shutdownCh:
			return
		}
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	if task.record.State != StateActive {
		log.Printf("task=%s restart cancelled (state=%s)", task.record.TaskID, task.record.State)
		return
	}

	var w *Worker
	var err error

	if h.actions != nil {
		handler, ok := h.actions[task.record.Action]
		if !ok {
			log.Printf("task=%s restart failed: unknown action %q", task.record.TaskID, task.record.Action)
			return
		}
		w, err = handler.Start(task.record.TaskID, task.record.Params, h.workerCBForTask(task))
	} else {
		log.Printf("task=%s restart failed: no actions configured", task.record.TaskID)
		return
	}

	if err != nil {
		log.Printf("task=%s restart failed: %v", task.record.TaskID, err)
		return
	}
	task.worker = w
	task.record.RestartCount = attempt
	now := time.Now().UTC()
	task.record.LastStartedAt = &now

	log.Printf("task=%s restarted attempt=%d new pid=%d", task.record.TaskID, attempt, w.PID)
	h.BroadcastToSubscribers(task.record.TaskID, StartedMessage{Type: "started", TaskID: task.record.TaskID, PID: w.PID, RestartOf: oldPID, TS: w.StartedAt})
}

// stopWorker stops the running worker for taskID.
// Retries bypass pool limits by design — the pool is only for admission of new starts.
// When intentional is true it also sets the task state to StateStopped.
// Called from pool excess-task handling and handleStop.
func (h *Hub) stopWorker(taskID string, intentional bool) {
	h.mu.RLock()
	task, ok := h.tasks[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	if intentional {
		task.record.State = StateStopped
	}

	if task.worker != nil {
		task.worker.mu.Lock()
		running := task.worker.State == WorkerRunning
		task.worker.mu.Unlock()
		if running {
			task.worker.Stop()
		}
	}
}

func taskInfo(task *Task) TaskInfo {
	r := task.record
	info := TaskInfo{
		TaskID:        r.TaskID,
		Action:        r.Action,
		Params:        r.Params,
		State:         r.State,
		RetryPolicy:   r.RetryPolicy,
		RestartCount:  r.RestartCount,
		CreatedAt:     r.CreatedAt,
		LastStartedAt: r.LastStartedAt,
		LastExitedAt:  r.LastExitedAt,
		ErrorMessage:  r.ErrorMessage,
	}
	if task.worker != nil {
		task.worker.mu.Lock()
		info.CurrentPID = task.worker.PID
		info.WorkerState = task.worker.State
		if task.worker.ExitCode != nil {
			ec := *task.worker.ExitCode
			info.LastExitCode = &ec
		}
		task.worker.mu.Unlock()
	}
	return info
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// sendJSON writes a single JSON message to one specific connection.
// It acquires the connection's write lock to prevent concurrent writes.
func (h *Hub) sendJSON(conn Conn, v interface{}) {
	mu := conn.WriteLock()
	mu.Lock()
	_ = conn.WriteJSON(v)
	mu.Unlock()
}

// sendError sends an error message to a single connection.
func (h *Hub) sendError(conn Conn, id, message string) {
	h.sendJSON(conn, ErrorMessage{Type: "error", ID: id, Message: message})
}

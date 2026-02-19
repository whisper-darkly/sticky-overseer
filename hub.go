package overseer

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// HubConfig holds all options for creating a Hub.
type HubConfig struct {
	DB            *sql.DB       // required; use OpenDB() to create
	PinnedCommand string        // optional; restricts start commands
	EventLog      *json.Encoder // optional; JSONL event log writer
}

// Task is the in-memory representation of a persistent task.
type Task struct {
	mu          sync.Mutex
	record      TaskRecord
	worker      *Worker     // current running worker, nil if not running
	exitHistory []time.Time // non-intentional exits for threshold tracking (not persisted)
}

type Hub struct {
	mu            sync.RWMutex
	clients       map[*websocket.Conn]*sync.Mutex // conn → per-conn write lock
	tasks         map[string]*Task                // task_id → Task
	db            *sql.DB
	pinnedCommand string
	eventLog      *json.Encoder
}

// NewHub creates a Hub, loads persisted tasks from DB, and marks them stopped.
func NewHub(cfg HubConfig) *Hub {
	h := &Hub{
		clients:       make(map[*websocket.Conn]*sync.Mutex),
		tasks:         make(map[string]*Task),
		db:            cfg.DB,
		pinnedCommand: cfg.PinnedCommand,
		eventLog:      cfg.EventLog,
	}
	if cfg.DB != nil {
		records, err := listTasks(cfg.DB)
		if err != nil {
			log.Printf("warn: failed to load tasks from db: %v", err)
		} else {
			for _, r := range records {
				r := r
				if r.State == "active" {
					r.State = "stopped"
					_ = updateTaskState(cfg.DB, r.TaskID, "stopped", "")
				}
				h.tasks[r.TaskID] = &Task{record: r}
			}
			log.Printf("loaded %d tasks from db (all marked stopped)", len(records))
		}
	}
	return h
}

// NewHandler returns an http.HandlerFunc that upgrades HTTP to WebSocket,
// enforces IP trust, and delegates to hub.HandleClient.
// Pass nil trustedNets to allow connections from any IP.
func NewHandler(h *Hub, trustedNets []*net.IPNet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isTrusted(r, trustedNets) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}
		log.Printf("client connected from %s", r.RemoteAddr)
		h.HandleClient(conn)
		log.Printf("client disconnected from %s", r.RemoteAddr)
	}
}

// ParseTrustedCIDRs parses a comma-separated list of bare IPs and CIDR ranges.
// Returns nil, nil for an empty string; callers interpret nil as "allow all".
func ParseTrustedCIDRs(s string) ([]*net.IPNet, error) {
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

// DetectLocalSubnets returns loopback (127.0.0.0/8, ::1/128) plus all
// subnets found on local network interfaces.
func DetectLocalSubnets() []*net.IPNet {
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

func (h *Hub) logEvent(v interface{}) {
	if h.eventLog == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	_ = h.eventLog.Encode(v)
}

// AddClient registers a WebSocket connection with the hub.
func (h *Hub) AddClient(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = &sync.Mutex{}
	h.mu.Unlock()
}

// RemoveClient deregisters a WebSocket connection.
func (h *Hub) RemoveClient(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

// Broadcast serialises msg to JSON and writes it to every connected client.
func (h *Hub) Broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn, mu := range h.clients {
		mu.Lock()
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("write error: %v", err)
		}
		mu.Unlock()
	}
}

// HandleClient runs the read loop for conn until the connection closes.
func (h *Hub) HandleClient(conn *websocket.Conn) {
	h.AddClient(conn)
	defer func() {
		h.RemoveClient(conn)
		conn.Close()
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg IncomingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "invalid JSON"})
			continue
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
		default:
			h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "unknown message type"})
		}
	}
}

func (h *Hub) handleStart(conn *websocket.Conn, msg IncomingMessage) {
	command := msg.Command
	if h.pinnedCommand != "" {
		if command != "" && command != h.pinnedCommand {
			h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: fmt.Sprintf("command must be %q (pinned)", h.pinnedCommand)})
			return
		}
		command = h.pinnedCommand
	}
	if command == "" {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "command is required"})
		return
	}
	args := msg.Args
	if args == nil {
		args = []string{}
	}

	taskID := msg.TaskID
	if taskID == "" {
		taskID = newUUID()
	}

	h.mu.Lock()
	task, exists := h.tasks[taskID]
	if !exists {
		now := time.Now().UTC()
		rec := TaskRecord{
			TaskID:      taskID,
			Command:     command,
			Args:        args,
			RetryPolicy: msg.RetryPolicy,
			State:       "active",
			CreatedAt:   now,
		}
		if h.db != nil {
			if err := createTask(h.db, rec); err != nil {
				h.mu.Unlock()
				h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "db error: " + err.Error()})
				return
			}
		}
		task = &Task{record: rec}
		h.tasks[taskID] = task
	}
	h.mu.Unlock()

	task.mu.Lock()
	defer task.mu.Unlock()

	if task.worker != nil {
		task.worker.mu.Lock()
		running := task.worker.State == "running"
		task.worker.mu.Unlock()
		if running {
			h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task already has a running worker"})
			return
		}
	}

	task.record.State = "active"
	w, err := StartWorker(h, WorkerConfig{TaskID: taskID, Command: command, Args: args})
	if err != nil {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	task.worker = w

	now := time.Now().UTC()
	task.record.LastStartedAt = &now
	if h.db != nil {
		_ = updateTaskState(h.db, taskID, "active", "")
		_ = updateTaskStats(h.db, taskID, task.record.RestartCount, task.record.ExitCount, &now, task.record.LastExitedAt)
	}

	log.Printf("started worker task=%s pid=%d cmd=%s", taskID, w.PID, command)
	started := StartedMessage{Type: "started", ID: msg.ID, TaskID: taskID, PID: w.PID, TS: w.StartedAt}
	h.sendJSON(conn, started)
	h.logEvent(started)
}

func (h *Hub) handleList(conn *websocket.Conn, msg IncomingMessage) {
	var since *time.Time
	if msg.Since != "" {
		t, err := time.Parse(time.RFC3339, msg.Since)
		if err != nil {
			h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "invalid since timestamp"})
			return
		}
		since = &t
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

	if infos == nil {
		infos = []TaskInfo{}
	}
	h.sendJSON(conn, TasksMessage{Type: "tasks", ID: msg.ID, Tasks: infos})
}

func (h *Hub) handleStop(conn *websocket.Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task_id is required"})
		return
	}
	h.mu.RLock()
	task, ok := h.tasks[msg.TaskID]
	h.mu.RUnlock()
	if !ok {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task not found"})
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	task.record.State = "stopped"
	if h.db != nil {
		_ = updateTaskState(h.db, msg.TaskID, "stopped", "")
	}

	if task.worker != nil {
		task.worker.mu.Lock()
		running := task.worker.State == "running"
		task.worker.mu.Unlock()
		if running {
			log.Printf("stopping worker task=%s pid=%d", msg.TaskID, task.worker.PID)
			task.worker.Stop()
		}
	}
}

func (h *Hub) handleReset(conn *websocket.Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task_id is required"})
		return
	}
	h.mu.RLock()
	task, ok := h.tasks[msg.TaskID]
	h.mu.RUnlock()
	if !ok {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task not found"})
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	if task.record.State != "errored" {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task is not in errored state"})
		return
	}

	task.record.State = "active"
	task.record.ExitCount = 0
	task.exitHistory = nil
	if h.db != nil {
		_ = updateTaskState(h.db, msg.TaskID, "active", "")
		_ = updateTaskStats(h.db, msg.TaskID, task.record.RestartCount, 0, task.record.LastStartedAt, task.record.LastExitedAt)
	}

	w, err := StartWorker(h, WorkerConfig{TaskID: task.record.TaskID, Command: task.record.Command, Args: task.record.Args})
	if err != nil {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	task.worker = w

	now := time.Now().UTC()
	task.record.LastStartedAt = &now
	if h.db != nil {
		_ = updateTaskStats(h.db, msg.TaskID, task.record.RestartCount, 0, &now, task.record.LastExitedAt)
	}

	log.Printf("reset task=%s, started pid=%d", msg.TaskID, w.PID)
	started := StartedMessage{Type: "started", ID: msg.ID, TaskID: msg.TaskID, PID: w.PID, TS: w.StartedAt}
	h.sendJSON(conn, started)
	h.logEvent(started)
}

func (h *Hub) handleReplay(conn *websocket.Conn, msg IncomingMessage) {
	if msg.TaskID == "" {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task_id is required"})
		return
	}
	h.mu.RLock()
	task, ok := h.tasks[msg.TaskID]
	h.mu.RUnlock()
	if !ok {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "task not found"})
		return
	}

	task.mu.Lock()
	w := task.worker
	task.mu.Unlock()

	if w == nil {
		h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "no worker for task"})
		return
	}

	var since *time.Time
	if msg.Since != "" {
		t, err := time.Parse(time.RFC3339, msg.Since)
		if err != nil {
			h.sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "invalid since timestamp"})
			return
		}
		since = &t
	}

	events := w.getEvents(since)
	for _, evt := range events {
		switch evt.Type {
		case "output":
			h.sendJSON(conn, OutputMessage{Type: "output", TaskID: evt.TaskID, PID: evt.PID, Stream: evt.Stream, Data: evt.Data, TS: evt.TS})
		case "exited":
			ec := 0
			if evt.ExitCode != nil {
				ec = *evt.ExitCode
			}
			h.sendJSON(conn, ExitedMessage{Type: "exited", TaskID: evt.TaskID, PID: evt.PID, ExitCode: ec, Intentional: evt.Intentional, TS: evt.TS})
		}
	}
}

// onWorkerExited is called by the worker goroutine when the process exits.
func (h *Hub) onWorkerExited(w *Worker, exitCode int, intentional bool, now time.Time) {
	exited := ExitedMessage{Type: "exited", TaskID: w.TaskID, PID: w.PID, ExitCode: exitCode, Intentional: intentional, TS: now}
	h.Broadcast(exited)
	h.logEvent(exited)

	h.mu.RLock()
	task, ok := h.tasks[w.TaskID]
	h.mu.RUnlock()
	if !ok {
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	task.record.LastExitedAt = &now
	if h.db != nil {
		_ = updateTaskStats(h.db, w.TaskID, task.record.RestartCount, task.record.ExitCount, task.record.LastStartedAt, &now)
	}

	if intentional || task.record.State == "stopped" {
		return
	}

	rp := task.record.RetryPolicy
	if rp == nil {
		return
	}

	if rp.ErrorWindow != "" {
		window, err := parseDuration(rp.ErrorWindow)
		if err == nil {
			cutoff := now.Add(-window)
			var pruned []time.Time
			for _, t := range task.exitHistory {
				if !t.Before(cutoff) {
					pruned = append(pruned, t)
				}
			}
			task.exitHistory = pruned
		}
	}
	task.exitHistory = append(task.exitHistory, now)
	task.record.ExitCount = len(task.exitHistory)

	if h.db != nil {
		_ = updateTaskStats(h.db, w.TaskID, task.record.RestartCount, task.record.ExitCount, task.record.LastStartedAt, &now)
	}

	if rp.ErrorThreshold > 0 && task.record.ExitCount >= rp.ErrorThreshold {
		task.record.State = "errored"
		if h.db != nil {
			_ = updateTaskState(h.db, w.TaskID, "errored", "exit threshold reached")
		}
		h.Broadcast(ErroredMessage{Type: "errored", TaskID: w.TaskID, PID: w.PID, ExitCount: task.record.ExitCount, TS: now})
		log.Printf("task=%s errored after %d exits", w.TaskID, task.record.ExitCount)
		return
	}

	restartDelay := 0 * time.Second
	if rp.RestartDelay != "" {
		if d, err := parseDuration(rp.RestartDelay); err == nil {
			restartDelay = d
		}
	}
	attempt := task.record.RestartCount + 1
	h.Broadcast(RestartingMessage{
		Type:         "restarting",
		TaskID:       w.TaskID,
		PID:          w.PID,
		RestartDelay: restartDelay.String(),
		Attempt:      attempt,
		TS:           now,
	})
	log.Printf("task=%s scheduling restart attempt=%d delay=%s", w.TaskID, attempt, restartDelay)
	go h.doRestart(task, w.PID, attempt, restartDelay)
}

func (h *Hub) doRestart(task *Task, oldPID int, attempt int, delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	if task.record.State != "active" {
		log.Printf("task=%s restart cancelled (state=%s)", task.record.TaskID, task.record.State)
		return
	}

	w, err := StartWorker(h, WorkerConfig{TaskID: task.record.TaskID, Command: task.record.Command, Args: task.record.Args})
	if err != nil {
		log.Printf("task=%s restart failed: %v", task.record.TaskID, err)
		return
	}
	task.worker = w
	task.record.RestartCount = attempt
	now := time.Now().UTC()
	task.record.LastStartedAt = &now
	if h.db != nil {
		_ = updateTaskStats(h.db, task.record.TaskID, attempt, task.record.ExitCount, &now, task.record.LastExitedAt)
	}

	log.Printf("task=%s restarted attempt=%d new pid=%d", task.record.TaskID, attempt, w.PID)
	h.Broadcast(StartedMessage{Type: "started", TaskID: task.record.TaskID, PID: w.PID, RestartOf: oldPID, TS: w.StartedAt})
}

func taskInfo(task *Task) TaskInfo {
	r := task.record
	info := TaskInfo{
		TaskID:        r.TaskID,
		Command:       r.Command,
		Args:          r.Args,
		State:         r.State,
		RetryPolicy:   r.RetryPolicy,
		RestartCount:  r.RestartCount,
		CreatedAt:     r.CreatedAt,
		LastStartedAt: r.LastStartedAt,
		LastExitedAt:  r.LastExitedAt,
	}
	if info.Args == nil {
		info.Args = []string{}
	}
	if task.worker != nil {
		task.worker.mu.Lock()
		info.CurrentPID = task.worker.PID
		info.WorkerState = task.worker.State
		task.worker.mu.Unlock()
	}
	return info
}

func parseDuration(s string) (time.Duration, error) {
	return time.ParseDuration(s)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (h *Hub) sendJSON(conn *websocket.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	mu := h.clients[conn]
	h.mu.RUnlock()
	if mu == nil {
		return
	}
	mu.Lock()
	conn.WriteMessage(websocket.TextMessage, data)
	mu.Unlock()
}

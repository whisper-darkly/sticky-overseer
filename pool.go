package overseer

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// QueueOrder determines the dequeue order for a TaskQueue.
type QueueOrder string

const (
	OrderFirst QueueOrder = "first" // FIFO — oldest item dequeued first
	OrderLast  QueueOrder = "last"  // LIFO — newest item dequeued first
)

// ExcessAction determines what happens to running tasks when pool limit decreases.
type ExcessAction string

const (
	ExcessStop    ExcessAction = "stop"    // SIGTERM excess running tasks
	ExcessRequeue ExcessAction = "requeue" // move newest running task back to queue
)

// KillOrder selects which running task to affect first when displacing or trimming.
type KillOrder string

const (
	KillFirst KillOrder = "first" // longest-running task first (oldest startedAt)
	KillLast  KillOrder = "last"  // shortest-running task first (newest startedAt)
)

// PoolConfig configures a pool layer (global or per-action).
type PoolConfig struct {
	Limit  int          `yaml:"limit"  json:"limit"`
	Queue  *QueueConfig `yaml:"queue"  json:"queue,omitempty"`
	Excess ExcessConfig `yaml:"excess" json:"excess,omitempty"`
}

// QueueConfig controls queuing when the pool limit is reached.
type QueueConfig struct {
	Enabled  bool           `yaml:"enabled"  json:"enabled"`
	Size     int            `yaml:"size"     json:"size"`
	Order    QueueOrder     `yaml:"order"    json:"order"`
	MaxAge   int            `yaml:"max_age"  json:"max_age"` // seconds; 0 = no expiry
	Displace DisplacePolicy `yaml:"displace" json:"displace"`
}

// ExcessConfig defines behaviour when a running limit is reduced at runtime.
type ExcessConfig struct {
	Action ExcessAction `yaml:"action" json:"action"`
	Order  KillOrder    `yaml:"order"  json:"order"`
	Grace  int          `yaml:"grace"  json:"grace"` // seconds before SIGKILL
}

// PoolInfo is the snapshot of pool state returned to clients.
type PoolInfo struct {
	Limit      int              `json:"limit"`
	Running    int              `json:"running"`
	QueueDepth int              `json:"queue_depth"`
	QueueItems []QueuedItemInfo `json:"queue_items,omitempty"`
}

// RunningEntry tracks an active task in the pool.
type RunningEntry struct {
	taskID    string
	action    string
	startedAt time.Time
	forced    bool
	stop      func() // cancels the running task (SIGTERM)
}

// QueuedItem holds a task waiting to start.
type QueuedItem struct {
	taskID     string
	action     string
	enqueuedAt time.Time
	forced     bool
	// startFn is called when the item is admitted to the pool.
	// It returns an error if the task could not be started.
	// For cancelled/expired/displaced items, startFn is NOT called;
	// use cancelFn to notify the waiter.
	startFn  func() error
	cancelFn func(reason error) // notifies the caller that the item was rejected
}

// QueuedItemInfo is the JSON-serialisable view of a QueuedItem.
type QueuedItemInfo struct {
	TaskID   string    `json:"task_id"`
	Action   string    `json:"action"`
	QueuedAt time.Time `json:"queued_at"`
	Position int       `json:"position"`
	Forced   bool      `json:"forced"`
}

// TaskQueue is a per-action FIFO or LIFO queue with optional age-based expiry.
type TaskQueue struct {
	cfg   *QueueConfig
	items []QueuedItem
	mu    sync.Mutex
}

// Enqueue adds an item. Returns false if the queue is full.
func (q *TaskQueue) Enqueue(item QueuedItem) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cfg.Size > 0 && len(q.items) >= q.cfg.Size {
		return false
	}
	q.items = append(q.items, item)
	return true
}

// Dequeue removes and returns the next item respecting FIFO/LIFO order.
// Expired items (maxAge > 0) are skipped lazily; their cancelFn is called
// after releasing the lock to prevent lock-order inversions.
// Returns nil if the queue is empty (after expiry filtering).
func (q *TaskQueue) Dequeue() *QueuedItem {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			return nil
		}

		var item QueuedItem
		if q.cfg.Order == OrderLast {
			item = q.items[len(q.items)-1]
			q.items = q.items[:len(q.items)-1]
		} else {
			item = q.items[0]
			q.items = q.items[1:]
		}
		q.mu.Unlock()

		// Lazy expiry check (outside lock to allow safe cancelFn calls).
		if q.cfg.MaxAge > 0 {
			cutoff := time.Now().Add(-time.Duration(q.cfg.MaxAge) * time.Second)
			if item.enqueuedAt.Before(cutoff) {
				if item.cancelFn != nil {
					item.cancelFn(errors.New("pool: item expired"))
				}
				continue // pick next item
			}
		}
		return &item
	}
}

// Remove removes the item with taskID from the queue.
// Returns (true, item) if found and removed.
func (q *TaskQueue) Remove(taskID string) (bool, *QueuedItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, item := range q.items {
		if item.taskID == taskID {
			removed := item
			q.items = append(q.items[:i], q.items[i+1:]...)
			return true, &removed
		}
	}
	return false, nil
}

// Info returns a snapshot of queue contents.
func (q *TaskQueue) Info() []QueuedItemInfo {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]QueuedItemInfo, len(q.items))
	for i, item := range q.items {
		out[i] = QueuedItemInfo{
			TaskID:   item.taskID,
			Action:   item.action,
			QueuedAt: item.enqueuedAt,
			Position: i,
			Forced:   item.forced,
		}
	}
	return out
}

// expireOld removes items older than maxAge seconds and returns them for notification.
func (q *TaskQueue) expireOld(maxAge int) []QueuedItem {
	if maxAge == 0 {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(maxAge) * time.Second)
	q.mu.Lock()
	var alive, expired []QueuedItem
	for _, item := range q.items {
		if item.enqueuedAt.Before(cutoff) {
			expired = append(expired, item)
		} else {
			alive = append(alive, item)
		}
	}
	q.items = alive
	q.mu.Unlock()
	return expired
}

// len returns the current number of items (under lock).
func (q *TaskQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// AcquireResult describes the outcome of a PoolManager.Acquire call.
type AcquireResult int

const (
	AcquireRunning AcquireResult = iota // task started immediately
	AcquireQueued                       // task placed in queue
)

// errPoolRejected is returned when a task cannot start or be queued.
var errPoolRejected = errors.New("pool: task rejected — limit reached and queue is full")

// PoolManager enforces concurrency limits, queuing, and displacement logic.
type PoolManager struct {
	mu          sync.Mutex
	globalCfg   PoolConfig
	actionCfgs  map[string]PoolConfig
	running     []RunningEntry
	queues      map[string]*TaskQueue
	actionOrder []string  // action names in order of last enqueue (for cross-action ordering)
	stopCh      chan struct{}
	tickInterval time.Duration // expiry sweep interval (computed from min MaxAge)
}

// NewPoolManager creates a PoolManager and starts the background expiry goroutine.
func NewPoolManager(globalCfg PoolConfig, actionCfgs map[string]PoolConfig) *PoolManager {
	if actionCfgs == nil {
		actionCfgs = make(map[string]PoolConfig)
	}
	pm := &PoolManager{
		globalCfg:    globalCfg,
		actionCfgs:   actionCfgs,
		queues:       make(map[string]*TaskQueue),
		stopCh:       make(chan struct{}),
		tickInterval: 5 * time.Second, // default; overridden by computeTickInterval
	}
	pm.tickInterval = pm.computeTickInterval()
	go pm.expiryLoop()
	return pm
}

// computeTickInterval returns half the smallest non-zero MaxAge across all
// queue configs, or 5s if no MaxAge is configured.
// Caller need not hold pm.mu (called only from NewPoolManager and SetLimits under lock).
func (pm *PoolManager) computeTickInterval() time.Duration {
	const defaultTick = 5 * time.Second
	min := 0
	check := func(age int) {
		if age > 0 && (min == 0 || age < min) {
			min = age
		}
	}
	if pm.globalCfg.Queue != nil {
		check(pm.globalCfg.Queue.MaxAge)
	}
	for _, ac := range pm.actionCfgs {
		if ac.Queue != nil {
			check(ac.Queue.MaxAge)
		}
	}
	if min == 0 {
		return defaultTick
	}
	d := time.Duration(min) * time.Second / 2
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// effectiveQueueConfig returns the QueueConfig for an action, falling back to global.
func (pm *PoolManager) effectiveQueueConfig(action string) *QueueConfig {
	if ac, ok := pm.actionCfgs[action]; ok && ac.Queue != nil {
		return ac.Queue
	}
	if pm.globalCfg.Queue != nil {
		return pm.globalCfg.Queue
	}
	return &QueueConfig{} // disabled
}

// queueFor returns (creating if needed) the TaskQueue for an action.
// Caller must hold pm.mu.
func (pm *PoolManager) queueFor(action string) *TaskQueue {
	q, ok := pm.queues[action]
	if !ok {
		cfg := pm.effectiveQueueConfig(action)
		q = &TaskQueue{cfg: cfg}
		pm.queues[action] = q
	}
	return q
}

// globalLimit returns the effective global pool limit (0 = unlimited).
func (pm *PoolManager) globalLimit() int {
	return pm.globalCfg.Limit
}

// actionLimit returns the per-action pool limit (0 = unlimited or use global).
func (pm *PoolManager) actionLimit(action string) int {
	if ac, ok := pm.actionCfgs[action]; ok {
		return ac.Limit
	}
	return 0
}

// runningCountForAction counts how many running entries match the given action.
// Caller must hold pm.mu.
func (pm *PoolManager) runningCountForAction(action string) int {
	n := 0
	for _, e := range pm.running {
		if e.action == action {
			n++
		}
	}
	return n
}

// hasCapacity returns true if starting another task with the given action is within
// both global and per-action limits. Caller must hold pm.mu.
func (pm *PoolManager) hasCapacity(action string) bool {
	globalLimit := pm.globalLimit()
	if globalLimit > 0 && len(pm.running) >= globalLimit {
		return false
	}
	actionLimit := pm.actionLimit(action)
	if actionLimit > 0 && pm.runningCountForAction(action) >= actionLimit {
		return false
	}
	return true
}

// Acquire attempts to start or queue a task.
//
// startFn is called synchronously when the task is admitted immediately; its error
// is propagated to the caller. cancelFn (may be nil) is called when the item is
// removed from the queue without being started (displacement, expiry, drain, purge).
// stopFn is stored for excess-task handling (SetLimits).
//
// Lock ordering: pm.mu is released BEFORE startFn is called. startFn may safely
// acquire other locks (e.g. h.mu in the hub) without creating a pm.mu → h.mu cycle.
// The running entry is pre-registered under pm.mu to prevent capacity overrun races.
func (pm *PoolManager) Acquire(
	taskID, action string,
	forced bool,
	stopFn func(),
	startFn func() error,
	cancelFn func(reason error),
) (AcquireResult, error) {
	pm.mu.Lock()

	if pm.hasCapacity(action) {
		// Pre-register as running UNDER pm.mu to prevent capacity overrun races
		// (another Acquire cannot fill this slot while we are calling startFn).
		pm.running = append(pm.running, RunningEntry{
			taskID:    taskID,
			action:    action,
			startedAt: time.Now(),
			forced:    forced,
			stop:      stopFn,
		})
		pm.mu.Unlock()

		// Call startFn OUTSIDE pm.mu so it can safely acquire h.mu.
		if err := startFn(); err != nil {
			// Start failed — remove the pre-registered entry.
			pm.mu.Lock()
			for i, r := range pm.running {
				if r.taskID == taskID {
					pm.running = append(pm.running[:i], pm.running[i+1:]...)
					break
				}
			}
			pm.mu.Unlock()
			return AcquireRunning, err
		}
		return AcquireRunning, nil
	}

	// Pool is full — try to queue.
	q := pm.queueFor(action)
	item := QueuedItem{
		taskID:     taskID,
		action:     action,
		enqueuedAt: time.Now(),
		forced:     forced,
		startFn:    startFn,
		cancelFn:   cancelFn,
	}

	if q.cfg.Enabled {
		if q.Enqueue(item) {
			pm.updateActionOrder(action)
			pm.mu.Unlock()
			return AcquireQueued, nil
		}
		// Queue full — try displacement.
		if ok, displaced := pm.tryDisplace(action, forced, item); ok {
			pm.mu.Unlock()
			// Call cancelFn OUTSIDE pm.mu to avoid lock-order inversion.
			if displaced != nil && displaced.cancelFn != nil {
				displaced.cancelFn(errors.New("pool: displaced by incoming task"))
			}
			return AcquireQueued, nil
		}
	}

	pm.mu.Unlock()
	return AcquireRunning, errPoolRejected
}

// tryDisplace attempts to remove an existing queued item to make room for newItem.
// Returns (true, displaced) if displacement succeeded and newItem was enqueued.
// The caller must invoke displaced.cancelFn AFTER releasing pm.mu to avoid
// lock-order inversions (pm.mu → h.mu via cancelFn → hub.Broadcast).
// Caller must hold pm.mu.
func (pm *PoolManager) tryDisplace(action string, forced bool, newItem QueuedItem) (bool, *QueuedItem) {
	// Build candidate list: own queue first, then others.
	candidates := []string{action}
	for a := range pm.queues {
		if a != action {
			candidates = append(candidates, a)
		}
	}

	for _, a := range candidates {
		q, ok := pm.queues[a]
		if !ok {
			continue
		}
		displace := pm.effectiveQueueConfig(a).Displace
		if displace == DisplaceProhibit {
			continue
		}
		if displace == DisplaceFalse && !forced {
			// Non-forced tasks may only displace if the queue explicitly allows it.
			continue
		}

		// Find the best non-forced item to displace (prefer non-forced over forced).
		q.mu.Lock()
		idx := -1
		for i, item := range q.items {
			if !item.forced {
				idx = i
				break
			}
		}
		// If forced task is arriving and all items are forced, we cannot displace.
		if idx < 0 {
			q.mu.Unlock()
			continue
		}
		displaced := q.items[idx]
		q.items = append(q.items[:idx], q.items[idx+1:]...)
		q.mu.Unlock()

		// Enqueue the new item (cancelFn for displaced is called by caller outside lock).
		newQ := pm.queueFor(newItem.action)
		newQ.Enqueue(newItem)
		pm.updateActionOrder(newItem.action)
		return true, &displaced
	}
	return false, nil
}

// Release marks a task as finished and admits the next queued task if any.
//
// The next queued task is pre-registered in pm.running WHILE holding pm.mu
// to prevent another Acquire() from filling the vacated slot before startFn
// completes (H3 pool-limit overrun fix). startFn is called outside pm.mu.
func (pm *PoolManager) Release(taskID string) {
	pm.mu.Lock()

	// Remove from running.
	for i, e := range pm.running {
		if e.taskID == taskID {
			pm.running = append(pm.running[:i], pm.running[i+1:]...)
			break
		}
	}

	// Dequeue the next task and pre-register it as running to reserve the slot.
	next := pm.nextQueued()
	if next != nil {
		pm.running = append(pm.running, RunningEntry{
			taskID:    next.taskID,
			action:    next.action,
			startedAt: time.Now(),
			forced:    next.forced,
			// stop will be set if/when startFn succeeds; nil here is acceptable.
		})
	}
	pm.mu.Unlock()

	if next != nil {
		// Call startFn OUTSIDE pm.mu so it can safely acquire h.mu.
		if err := next.startFn(); err != nil {
			// Start failed — roll back the pre-registered entry and notify caller.
			pm.mu.Lock()
			for i, r := range pm.running {
				if r.taskID == next.taskID {
					pm.running = append(pm.running[:i], pm.running[i+1:]...)
					break
				}
			}
			pm.mu.Unlock()
			if next.cancelFn != nil {
				next.cancelFn(err)
			}
		}
	}
}

// nextQueued returns (and removes) the next eligible item from any queue, respecting
// global actionOrder. Items that are expired (lazy check) are skipped.
// Caller must hold pm.mu.
func (pm *PoolManager) nextQueued() *QueuedItem {
	// Try actions in actionOrder first.
	for _, action := range pm.actionOrder {
		q, ok := pm.queues[action]
		if !ok {
			continue
		}
		item := q.Dequeue() // Dequeue performs lazy expiry
		if item != nil {
			return item
		}
	}
	// Fallback: try any queue in map order.
	for _, q := range pm.queues {
		item := q.Dequeue()
		if item != nil {
			return item
		}
	}
	return nil
}

// updateActionOrder moves action to the front of actionOrder (most-recently-enqueued).
// Caller must hold pm.mu.
func (pm *PoolManager) updateActionOrder(action string) {
	for i, a := range pm.actionOrder {
		if a == action {
			pm.actionOrder = append(pm.actionOrder[:i], pm.actionOrder[i+1:]...)
			break
		}
	}
	pm.actionOrder = append([]string{action}, pm.actionOrder...)
}

// Dequeue removes a specific task from any queue by taskID. Returns true if found.
// cancelFn is called AFTER releasing pm.mu to avoid lock-order inversions.
func (pm *PoolManager) Dequeue(taskID string) bool {
	pm.mu.Lock()
	var found *QueuedItem
	for _, q := range pm.queues {
		if ok, item := q.Remove(taskID); ok {
			found = item
			break
		}
	}
	pm.mu.Unlock() // release BEFORE calling cancelFn

	if found != nil {
		if found.cancelFn != nil {
			found.cancelFn(errors.New("pool: task dequeued"))
		}
		return true
	}
	return false
}

// PurgeQueue removes all queued items for an action (or all queues if action is "").
// cancelFn is called for each purged item. Returns the number of items purged.
func (pm *PoolManager) PurgeQueue(action string) (int, []string) {
	pm.mu.Lock()

	var count int
	var taskIDs []string
	var toPurge []QueuedItem

	doPurge := func(q *TaskQueue) {
		q.mu.Lock()
		items := make([]QueuedItem, len(q.items))
		copy(items, q.items)
		q.items = nil
		q.mu.Unlock()
		toPurge = append(toPurge, items...)
		for _, item := range items {
			taskIDs = append(taskIDs, item.taskID)
			count++
		}
	}

	if action == "" {
		for _, q := range pm.queues {
			doPurge(q)
		}
	} else if q, ok := pm.queues[action]; ok {
		doPurge(q)
	}

	pm.mu.Unlock()

	// Notify outside the lock.
	for _, item := range toPurge {
		if item.cancelFn != nil {
			item.cancelFn(errors.New("pool: queue purged"))
		}
	}
	return count, taskIDs
}

// SetLimits dynamically updates the pool limit and queue size for an action (or global).
// If the new limit is lower than the current running count, excess tasks are handled
// according to the ExcessConfig.
func (pm *PoolManager) SetLimits(action string, limit, queueSize int, excess *ExcessConfig) error {
	pm.mu.Lock()

	if action == "" {
		pm.globalCfg.Limit = limit
		if pm.globalCfg.Queue == nil {
			pm.globalCfg.Queue = &QueueConfig{}
		}
		pm.globalCfg.Queue.Size = queueSize
		if excess != nil {
			pm.globalCfg.Excess = *excess
		}
	} else {
		ac := pm.actionCfgs[action]
		ac.Limit = limit
		if ac.Queue == nil {
			ac.Queue = &QueueConfig{}
		}
		ac.Queue.Size = queueSize
		if excess != nil {
			ac.Excess = *excess
		}
		pm.actionCfgs[action] = ac
	}

	// Recompute tick interval in case MaxAge config changed.
	pm.tickInterval = pm.computeTickInterval()

	// Determine effective excess config and running entries that are now over-limit.
	var effectiveExcess ExcessConfig
	if action == "" {
		effectiveExcess = pm.globalCfg.Excess
	} else {
		effectiveExcess = pm.actionCfgs[action].Excess
	}

	// Find running entries affected by the limit reduction.
	var candidates []RunningEntry
	if action == "" {
		if limit > 0 && len(pm.running) > limit {
			candidates = make([]RunningEntry, len(pm.running))
			copy(candidates, pm.running)
		}
	} else {
		for _, e := range pm.running {
			if e.action == action {
				candidates = append(candidates, e)
			}
		}
		if limit == 0 || len(candidates) <= limit {
			candidates = nil
		}
	}

	var toStop []RunningEntry
	var toRequeue []RunningEntry

	if len(candidates) > 0 {
		// Sort candidates by startedAt to apply KillOrder.
		sort.Slice(candidates, func(i, j int) bool {
			if effectiveExcess.Order == KillLast {
				return candidates[i].startedAt.After(candidates[j].startedAt)
			}
			return candidates[i].startedAt.Before(candidates[j].startedAt)
		})

		runningCount := len(pm.running)
		if action != "" {
			runningCount = 0
			for _, e := range pm.running {
				if e.action == action {
					runningCount++
				}
			}
		}
		excess_ := runningCount - limit
		if excess_ < 0 {
			excess_ = 0
		}

		for i := 0; i < excess_ && i < len(candidates); i++ {
			// ExcessRequeue is not yet fully implemented: storing the original startFn
			// in RunningEntry is required to re-admit queued items properly.
			// Until then, treat ExcessRequeue as ExcessStop to prevent a nil startFn
			// panic in Release() (C1 fix).
			toStop = append(toStop, candidates[i])
		}
	}

	pm.mu.Unlock()

	// Stop outside the lock.
	for _, e := range toStop {
		if e.stop != nil {
			e.stop()
		}
		pm.Release(e.taskID)
	}
	_ = toRequeue // ExcessRequeue collapsed into toStop above; toRequeue is always empty.

	return nil
}

// Info returns pool state for an action (or global if action is "").
func (pm *PoolManager) Info(action string) PoolInfo {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	info := PoolInfo{Limit: pm.globalCfg.Limit}
	info.Running = len(pm.running)

	if action != "" {
		runCount := 0
		for _, e := range pm.running {
			if e.action == action {
				runCount++
			}
		}
		info.Running = runCount
		if q, ok := pm.queues[action]; ok {
			items := q.Info()
			info.QueueDepth = len(items)
			info.QueueItems = items
		}
		if ac, ok := pm.actionCfgs[action]; ok {
			info.Limit = ac.Limit
		}
	} else {
		for _, q := range pm.queues {
			items := q.Info()
			info.QueueDepth += len(items)
			info.QueueItems = append(info.QueueItems, items...)
		}
	}
	return info
}

// DrainAll notifies all queued tasks of shutdown and clears internal state.
// It signals the background expiry goroutine to stop.
func (pm *PoolManager) DrainAll() {
	pm.mu.Lock()
	var queued []QueuedItem
	for _, q := range pm.queues {
		q.mu.Lock()
		queued = append(queued, q.items...)
		q.items = nil
		q.mu.Unlock()
	}
	pm.running = nil
	pm.mu.Unlock()

	for _, item := range queued {
		if item.cancelFn != nil {
			item.cancelFn(errors.New("pool: shutdown"))
		}
	}

	// Signal expiry loop to stop (only close once).
	select {
	case <-pm.stopCh:
		// already closed
	default:
		close(pm.stopCh)
	}
}

// expiryLoop runs in the background and expires old queued items periodically.
func (pm *PoolManager) expiryLoop() {
	pm.mu.Lock()
	interval := pm.tickInterval
	pm.mu.Unlock()

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			pm.sweepExpired()
			// Re-read interval in case SetLimits changed it.
			pm.mu.Lock()
			newInterval := pm.tickInterval
			pm.mu.Unlock()
			if newInterval != interval {
				interval = newInterval
				tick.Reset(interval)
			}
		case <-pm.stopCh:
			return
		}
	}
}

// sweepExpired iterates all queues and expires items older than their MaxAge.
func (pm *PoolManager) sweepExpired() {
	pm.mu.Lock()
	// Snapshot queue map to avoid holding pm.mu during cancelFn calls.
	queues := make(map[string]*TaskQueue, len(pm.queues))
	for k, v := range pm.queues {
		queues[k] = v
	}
	pm.mu.Unlock()

	for _, q := range queues {
		expired := q.expireOld(q.cfg.MaxAge)
		for _, item := range expired {
			if item.cancelFn != nil {
				item.cancelFn(errors.New("pool: item expired"))
			}
		}
	}
}

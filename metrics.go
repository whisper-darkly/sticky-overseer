package overseer

import (
	"sync"
	"sync/atomic"
)

// TaskMetrics holds lifetime stats for a single task.
type TaskMetrics struct {
	OutputLines  int64 `json:"output_lines"`
	RestartCount int64 `json:"restart_count"`
	RuntimeMs    int64 `json:"runtime_ms"`
	LastExitCode int   `json:"last_exit_code"`
}

// ActionMetrics holds aggregate stats for all tasks of one action type.
type ActionMetrics struct {
	TasksStarted     int64 `json:"tasks_started"`
	TasksCompleted   int64 `json:"tasks_completed"`
	TasksErrored     int64 `json:"tasks_errored"`
	TasksRestarted   int64 `json:"tasks_restarted"`
	TotalOutputLines int64 `json:"total_output_lines"`
}

// GlobalMetricsSnapshot is a point-in-time copy of the global counters.
// Defined in messages.go for JSON serialisation alongside other message types.

// Metrics is the central in-memory metrics registry. All state is reset on
// process restart. Global counters use sync/atomic for low-overhead hot-path
// writes (RecordOutput is called once per output line). Per-action and per-task
// maps are protected by mu.
type Metrics struct {
	mu sync.RWMutex

	// Global counters — use atomic for hot-path performance.
	tasksStarted     atomic.Int64
	tasksCompleted   atomic.Int64
	tasksErrored     atomic.Int64
	tasksRestarted   atomic.Int64
	totalOutputLines atomic.Int64

	// Queue counters.
	enqueued  atomic.Int64
	dequeued  atomic.Int64
	displaced atomic.Int64
	expired   atomic.Int64

	// Per-action aggregates (keyed by action name).
	byAction map[string]*ActionMetrics

	// Per-task stats (keyed by task_id); capped at maxTaskEntries.
	byTask    map[string]*TaskMetrics
	taskOrder []string // insertion order for LRU eviction
}

const maxTaskEntries = 10000

// NewMetrics creates an empty Metrics registry.
func NewMetrics() *Metrics {
	return &Metrics{
		byAction: make(map[string]*ActionMetrics),
		byTask:   make(map[string]*TaskMetrics),
	}
}

// actionEntry returns (creating if needed) the ActionMetrics for an action.
// Caller must hold m.mu (write lock).
func (m *Metrics) actionEntry(action string) *ActionMetrics {
	e, ok := m.byAction[action]
	if !ok {
		e = &ActionMetrics{}
		m.byAction[action] = e
	}
	return e
}

// taskEntry returns (creating if needed) the TaskMetrics for a taskID.
// Caller must hold m.mu (write lock).
func (m *Metrics) taskEntry(taskID string) *TaskMetrics {
	e, ok := m.byTask[taskID]
	if !ok {
		e = &TaskMetrics{}
		m.byTask[taskID] = e
		m.taskOrder = append(m.taskOrder, taskID)
		// Evict oldest entry if we exceed the cap.
		for len(m.byTask) > maxTaskEntries && len(m.taskOrder) > 0 {
			oldest := m.taskOrder[0]
			m.taskOrder = m.taskOrder[1:]
			delete(m.byTask, oldest)
		}
	}
	return e
}

// RecordStart increments global + per-action started counters and initialises
// the per-task entry.
func (m *Metrics) RecordStart(taskID, action string) {
	m.tasksStarted.Add(1)

	m.mu.Lock()
	m.actionEntry(action).TasksStarted++
	m.taskEntry(taskID) // ensure entry exists
	m.mu.Unlock()
}

// RecordExit updates completed/errored counters, runtime, and last exit code.
// runtimeMs is the worker's wall-clock runtime in milliseconds.
func (m *Metrics) RecordExit(taskID, action string, exitCode int, errored bool, runtimeMs int64) {
	m.tasksCompleted.Add(1)
	if errored {
		m.tasksErrored.Add(1)
	}

	m.mu.Lock()
	ae := m.actionEntry(action)
	ae.TasksCompleted++
	if errored {
		ae.TasksErrored++
	}

	te := m.taskEntry(taskID)
	te.LastExitCode = exitCode
	te.RuntimeMs += runtimeMs
	m.mu.Unlock()
}

// RecordRestart increments global + per-action + per-task restart counters.
func (m *Metrics) RecordRestart(taskID, action string) {
	m.tasksRestarted.Add(1)

	m.mu.Lock()
	m.actionEntry(action).TasksRestarted++
	m.taskEntry(taskID).RestartCount++
	m.mu.Unlock()
}

// RecordOutput increments global + per-action + per-task output line counters.
// Hot path — global counter uses atomic to minimise lock contention.
func (m *Metrics) RecordOutput(taskID, action string) {
	m.totalOutputLines.Add(1)

	m.mu.Lock()
	m.actionEntry(action).TotalOutputLines++
	m.taskEntry(taskID).OutputLines++
	m.mu.Unlock()
}

// RecordEnqueued increments the global enqueued counter.
func (m *Metrics) RecordEnqueued() { m.enqueued.Add(1) }

// RecordDequeued increments the global dequeued counter.
func (m *Metrics) RecordDequeued() { m.dequeued.Add(1) }

// RecordDisplaced increments the global displaced counter.
func (m *Metrics) RecordDisplaced() { m.displaced.Add(1) }

// RecordExpired increments the global expired counter.
func (m *Metrics) RecordExpired() { m.expired.Add(1) }

// GlobalSnapshot returns an atomic-consistent snapshot of the global counters.
func (m *Metrics) GlobalSnapshot() GlobalMetricsSnapshot {
	return GlobalMetricsSnapshot{
		TasksStarted:     m.tasksStarted.Load(),
		TasksCompleted:   m.tasksCompleted.Load(),
		TasksErrored:     m.tasksErrored.Load(),
		TasksRestarted:   m.tasksRestarted.Load(),
		TotalOutputLines: m.totalOutputLines.Load(),
		Enqueued:         m.enqueued.Load(),
		Dequeued:         m.dequeued.Load(),
		Displaced:        m.displaced.Load(),
		Expired:          m.expired.Load(),
	}
}

// ActionSnapshot returns a copy of per-action counters, or nil if the action
// has never been recorded.
func (m *Metrics) ActionSnapshot(action string) *ActionMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byAction[action]
	if !ok {
		return nil
	}
	copy := *e
	return &copy
}

// TaskSnapshot returns a copy of per-task counters, or nil if the task has
// never been recorded.
func (m *Metrics) TaskSnapshot(taskID string) *TaskMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byTask[taskID]
	if !ok {
		return nil
	}
	copy := *e
	return &copy
}

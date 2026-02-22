package overseer

import (
	"encoding/json"
	"time"

	"gopkg.in/yaml.v3"
)

// TaskState represents the lifecycle state of a task.
type TaskState string

const (
	StateActive  TaskState = "active"
	StateStopped TaskState = "stopped"
	StateErrored TaskState = "errored"
)

// WorkerState represents the running state of a worker process.
type WorkerState string

const (
	WorkerRunning WorkerState = "running"
	WorkerExited  WorkerState = "exited"
)

// Stream identifies the output stream of a worker process.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// RetryPolicy controls automatic restarts.
type RetryPolicy struct {
	RestartDelay   time.Duration `json:"restart_delay,omitempty"`
	ErrorWindow    time.Duration `json:"error_window,omitempty"`
	ErrorThreshold int           `json:"error_threshold,omitempty"`
}

// MarshalJSON serialises RestartDelay and ErrorWindow as human-readable
// duration strings (e.g. "30s") rather than the default int64 nanoseconds.
func (r RetryPolicy) MarshalJSON() ([]byte, error) {
	type raw struct {
		RestartDelay   string `json:"restart_delay,omitempty"`
		ErrorWindow    string `json:"error_window,omitempty"`
		ErrorThreshold int    `json:"error_threshold,omitempty"`
	}
	v := raw{ErrorThreshold: r.ErrorThreshold}
	if r.RestartDelay != 0 {
		v.RestartDelay = r.RestartDelay.String()
	}
	if r.ErrorWindow != 0 {
		v.ErrorWindow = r.ErrorWindow.String()
	}
	return json.Marshal(v)
}

// UnmarshalYAML parses RestartDelay and ErrorWindow from duration strings in YAML.
func (r *RetryPolicy) UnmarshalYAML(value *yaml.Node) error {
	type raw struct {
		RestartDelay   string `yaml:"restart_delay"`
		ErrorWindow    string `yaml:"error_window"`
		ErrorThreshold int    `yaml:"error_threshold"`
	}
	var v raw
	if err := value.Decode(&v); err != nil {
		return err
	}
	r.ErrorThreshold = v.ErrorThreshold
	if v.RestartDelay != "" {
		d, err := time.ParseDuration(v.RestartDelay)
		if err != nil {
			return err
		}
		r.RestartDelay = d
	}
	if v.ErrorWindow != "" {
		d, err := time.ParseDuration(v.ErrorWindow)
		if err != nil {
			return err
		}
		r.ErrorWindow = d
	}
	return nil
}

// UnmarshalJSON parses RestartDelay and ErrorWindow from duration strings at
// decode time, so a malformed value is caught immediately rather than silently
// disabling the feature.
func (r *RetryPolicy) UnmarshalJSON(b []byte) error {
	type raw struct {
		RestartDelay   string `json:"restart_delay,omitempty"`
		ErrorWindow    string `json:"error_window,omitempty"`
		ErrorThreshold int    `json:"error_threshold,omitempty"`
	}
	var v raw
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	r.ErrorThreshold = v.ErrorThreshold
	if v.RestartDelay != "" {
		d, err := time.ParseDuration(v.RestartDelay)
		if err != nil {
			return err
		}
		r.RestartDelay = d
	}
	if v.ErrorWindow != "" {
		d, err := time.ParseDuration(v.ErrorWindow)
		if err != nil {
			return err
		}
		r.ErrorWindow = d
	}
	return nil
}

// StateQueued is a task state for tasks accepted but waiting in the pool queue.
const StateQueued TaskState = "queued"

// Client → Server messages

type IncomingMessage struct {
	Type        string            `json:"type"`
	ID          string            `json:"id,omitempty"`
	TaskID      string            `json:"task_id,omitempty"`
	Action      string            `json:"action,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	Force       bool              `json:"force,omitempty"`
	Limit       int               `json:"limit,omitempty"`
	QueueSize   int               `json:"queue_size,omitempty"`
	Excess      *ExcessConfig     `json:"excess,omitempty"`
	RetryPolicy *RetryPolicy      `json:"retry_policy,omitempty"`
	Since       string            `json:"since,omitempty"`
}

// Server → Client messages

type StartedMessage struct {
	Type      string    `json:"type"`
	ID        string    `json:"id,omitempty"`
	TaskID    string    `json:"task_id"`
	PID       int       `json:"pid"`
	RestartOf int       `json:"restart_of,omitempty"`
	TS        time.Time `json:"ts"`
}

type TaskInfo struct {
	TaskID        string            `json:"task_id"`
	Action        string            `json:"action"`
	Params        map[string]string `json:"params,omitempty"`
	QueuedAt      *time.Time        `json:"queued_at,omitempty"`
	State         TaskState         `json:"state"`
	RetryPolicy   *RetryPolicy      `json:"retry_policy,omitempty"`
	CurrentPID    int               `json:"current_pid,omitempty"`
	WorkerState   WorkerState       `json:"worker_state,omitempty"`
	RestartCount  int               `json:"restart_count"`
	CreatedAt     time.Time         `json:"created_at"`
	LastStartedAt *time.Time        `json:"last_started_at,omitempty"`
	LastExitedAt  *time.Time        `json:"last_exited_at,omitempty"`
	LastExitCode  *int              `json:"last_exit_code,omitempty"`
	ErrorMessage  string            `json:"error_message,omitempty"`
}

type TasksMessage struct {
	Type  string     `json:"type"`
	ID    string     `json:"id,omitempty"`
	Tasks []TaskInfo `json:"tasks"`
}

type OutputMessage struct {
	Type   string    `json:"type"`
	TaskID string    `json:"task_id"`
	PID    int       `json:"pid"`
	Stream Stream    `json:"stream"`
	Data   string    `json:"data"`
	TS     time.Time `json:"ts"`
	Seq    int64     `json:"seq,omitempty"` // per-task monotonic sequence number; 0 = not set
}

type ExitedMessage struct {
	Type        string    `json:"type"`
	TaskID      string    `json:"task_id"`
	PID         int       `json:"pid"`
	ExitCode    int       `json:"exit_code"`
	Intentional bool      `json:"intentional"`
	TS          time.Time `json:"ts"`
}

// RestartingMessage is broadcast when a task is scheduled for restart.
// RestartDelay is serialised as a human-readable duration string.
type RestartingMessage struct {
	Type         string
	TaskID       string
	PID          int
	RestartDelay time.Duration
	Attempt      int
	TS           time.Time
}

// MarshalJSON serialises RestartingMessage with RestartDelay as a string.
func (m RestartingMessage) MarshalJSON() ([]byte, error) {
	type raw struct {
		Type         string    `json:"type"`
		TaskID       string    `json:"task_id"`
		PID          int       `json:"pid"`
		RestartDelay string    `json:"restart_delay"`
		Attempt      int       `json:"attempt"`
		TS           time.Time `json:"ts"`
	}
	return json.Marshal(raw{
		Type:         m.Type,
		TaskID:       m.TaskID,
		PID:          m.PID,
		RestartDelay: m.RestartDelay.String(),
		Attempt:      m.Attempt,
		TS:           m.TS,
	})
}

type ErroredMessage struct {
	Type      string    `json:"type"`
	TaskID    string    `json:"task_id"`
	PID       int       `json:"pid"`
	ExitCount int       `json:"exit_count"`
	TS        time.Time `json:"ts"`
}

type ErrorMessage struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	Message        string `json:"message"`
	ExistingTaskID string `json:"existing_task_id,omitempty"` // set on duplicate task detection
}

// New server→client message types for action/pool model.

// QueuedMessage is broadcast when a task is accepted but placed in the queue.
type QueuedMessage struct {
	Type     string    `json:"type"`
	ID       string    `json:"id,omitempty"`
	TaskID   string    `json:"task_id"`
	Action   string    `json:"action"`
	Position int       `json:"position"`
	TS       time.Time `json:"ts"`
}

// DequeuedMessage is broadcast when a queued task is removed (stopped or expired).
type DequeuedMessage struct {
	Type   string    `json:"type"`
	TaskID string    `json:"task_id"`
	Reason string    `json:"reason,omitempty"`
	TS     time.Time `json:"ts"`
}

// ActionsMessage is the response to a "describe" request.
// ActionInfo is defined in actions.go.
type ActionsMessage struct {
	Type    string       `json:"type"`
	ID      string       `json:"id,omitempty"`
	Actions []ActionInfo `json:"actions"`
}

// PoolMessage is the response to a "pool_info" request.
// PoolInfo is defined in pool.go.
type PoolMessage struct {
	Type   string   `json:"type"`
	ID     string   `json:"id,omitempty"`
	Action string   `json:"action,omitempty"`
	Pool   PoolInfo `json:"pool"`
}

// PurgedMessage is sent after a successful "purge" request.
type PurgedMessage struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Action string `json:"action,omitempty"`
	Count  int    `json:"count"`
}

// PoolUpdatedMessage is broadcast when pool limits change via "set_pool".
type PoolUpdatedMessage struct {
	Type   string   `json:"type"`
	Action string   `json:"action,omitempty"`
	Pool   PoolInfo `json:"pool"`
}

// GlobalMetricsSnapshot is a point-in-time copy of the global counters returned
// by the "metrics" WebSocket message handler.
type GlobalMetricsSnapshot struct {
	TasksStarted     int64 `json:"tasks_started"`
	TasksCompleted   int64 `json:"tasks_completed"`
	TasksErrored     int64 `json:"tasks_errored"`
	TasksRestarted   int64 `json:"tasks_restarted"`
	TotalOutputLines int64 `json:"total_output_lines"`
	Enqueued         int64 `json:"enqueued"`
	Dequeued         int64 `json:"dequeued"`
	Displaced        int64 `json:"displaced"`
	Expired          int64 `json:"expired"`
}

// MetricsMessage is the response to a "metrics" WebSocket request.
// Exactly one of Global, Action, or Task will be non-nil depending on the
// requested granularity (default=global, action=per-action, task_id=per-task).
type MetricsMessage struct {
	Type   string                 `json:"type"`             // "metrics"
	ID     string                 `json:"id,omitempty"`
	Global *GlobalMetricsSnapshot `json:"global,omitempty"`
	Action *ActionMetrics         `json:"action,omitempty"`
	Task   *TaskMetrics           `json:"task,omitempty"`
}

// SubscribedMessage confirms a successful subscribe request.
type SubscribedMessage struct {
	Type   string `json:"type"`   // "subscribed"
	ID     string `json:"id,omitempty"`
	TaskID string `json:"task_id"`
}

// UnsubscribedMessage confirms a successful unsubscribe request.
type UnsubscribedMessage struct {
	Type   string `json:"type"`   // "unsubscribed"
	ID     string `json:"id,omitempty"`
	TaskID string `json:"task_id"`
}

// ManifestWireMessage is the WS response to a "manifest" client request.
type ManifestWireMessage struct {
	Type     string     `json:"type"`
	ID       string     `json:"id,omitempty"`
	Manifest WSManifest `json:"manifest"`
}

// Event stored in ring buffer

type Event struct {
	Type        string    `json:"type"`
	TaskID      string    `json:"task_id"`
	PID         int       `json:"pid"`
	Stream      Stream    `json:"stream,omitempty"`
	Data        string    `json:"data,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Intentional bool      `json:"intentional,omitempty"`
	TS          time.Time `json:"ts"`
	Seq         int64     `json:"seq,omitempty"` // per-task monotonic sequence number carried through ring buffer
}

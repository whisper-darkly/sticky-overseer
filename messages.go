package overseer

import (
	"encoding/json"
	"time"
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

// Client → Server messages

type IncomingMessage struct {
	Type        string       `json:"type"`
	ID          string       `json:"id,omitempty"`
	TaskID      string       `json:"task_id,omitempty"`
	Command     string       `json:"command,omitempty"`
	Args        []string     `json:"args,omitempty"`
	RetryPolicy *RetryPolicy `json:"retry_policy,omitempty"`
	Since       string       `json:"since,omitempty"`
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
	TaskID        string       `json:"task_id"`
	Command       string       `json:"command"`
	Args          []string     `json:"args"`
	State         TaskState    `json:"state"`
	RetryPolicy   *RetryPolicy `json:"retry_policy,omitempty"`
	CurrentPID    int          `json:"current_pid,omitempty"`
	WorkerState   WorkerState  `json:"worker_state,omitempty"`
	RestartCount  int          `json:"restart_count"`
	CreatedAt     time.Time    `json:"created_at"`
	LastStartedAt *time.Time   `json:"last_started_at,omitempty"`
	LastExitedAt  *time.Time   `json:"last_exited_at,omitempty"`
	LastExitCode  *int         `json:"last_exit_code,omitempty"`
	ErrorMessage  string       `json:"error_message,omitempty"`
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
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
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
}

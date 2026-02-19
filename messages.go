package main

import "time"

// RetryPolicy controls automatic restarts.
type RetryPolicy struct {
	RestartDelay   string `json:"restart_delay,omitempty"`   // e.g. "30s"
	ErrorWindow    string `json:"error_window,omitempty"`    // e.g. "5m"
	ErrorThreshold int    `json:"error_threshold,omitempty"` // max non-intentional exits in window
}

// Client → Server messages

type IncomingMessage struct {
	Type        string      `json:"type"`
	ID          string      `json:"id,omitempty"`
	TaskID      string      `json:"task_id,omitempty"`
	Command     string      `json:"command,omitempty"`
	Args        []string    `json:"args,omitempty"`
	RetryPolicy *RetryPolicy `json:"retry_policy,omitempty"`
	Since       string      `json:"since,omitempty"`
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
	State         string       `json:"state"`
	RetryPolicy   *RetryPolicy `json:"retry_policy,omitempty"`
	CurrentPID    int          `json:"current_pid,omitempty"`
	WorkerState   string       `json:"worker_state,omitempty"`
	RestartCount  int          `json:"restart_count"`
	CreatedAt     time.Time    `json:"created_at"`
	LastStartedAt *time.Time   `json:"last_started_at,omitempty"`
	LastExitedAt  *time.Time   `json:"last_exited_at,omitempty"`
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
	Stream string    `json:"stream"`
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

type RestartingMessage struct {
	Type         string    `json:"type"`
	TaskID       string    `json:"task_id"`
	PID          int       `json:"pid"`
	RestartDelay string    `json:"restart_delay"`
	Attempt      int       `json:"attempt"`
	TS           time.Time `json:"ts"`
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
	Stream      string    `json:"stream,omitempty"`
	Data        string    `json:"data,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Intentional bool      `json:"intentional,omitempty"`
	TS          time.Time `json:"ts"`
}

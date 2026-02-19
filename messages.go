package main

import "time"

// Client → Server messages

type IncomingMessage struct {
	Type    string   `json:"type"`
	ID      string   `json:"id,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	PID     int      `json:"pid,omitempty"`
	Since   string   `json:"since,omitempty"`
}

// Server → Client messages

type StartedMessage struct {
	Type string    `json:"type"`
	ID   string    `json:"id"`
	PID  int       `json:"pid"`
	TS   time.Time `json:"ts"`
}

type WorkerInfo struct {
	PID         int       `json:"pid"`
	Command     string    `json:"command"`
	Args        []string  `json:"args"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	ExitedAt    *time.Time `json:"exited_at,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
}

type WorkersMessage struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Workers []WorkerInfo `json:"workers"`
}

type OutputMessage struct {
	Type   string    `json:"type"`
	PID    int       `json:"pid"`
	Stream string    `json:"stream"`
	Data   string    `json:"data"`
	TS     time.Time `json:"ts"`
}

type ExitedMessage struct {
	Type     string    `json:"type"`
	PID      int       `json:"pid"`
	ExitCode int       `json:"exit_code"`
	TS       time.Time `json:"ts"`
}

type ErrorMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
}

// Event stored in ring buffer
type Event struct {
	Type     string    `json:"type"`
	PID      int       `json:"pid"`
	Stream   string    `json:"stream,omitempty"`
	Data     string    `json:"data,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	TS       time.Time `json:"ts"`
}

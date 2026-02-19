package overseer

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"
)

type TaskRecord struct {
	TaskID        string
	Command       string
	Args          []string
	RetryPolicy   *RetryPolicy
	State         string // "active", "stopped", "errored"
	RestartCount  int
	ExitCount     int
	CreatedAt     time.Time
	LastStartedAt *time.Time
	LastExitedAt  *time.Time
	ErrorMessage  string
}

func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		task_id         TEXT PRIMARY KEY,
		command         TEXT NOT NULL,
		args            TEXT NOT NULL,
		retry_policy    TEXT,
		state           TEXT NOT NULL DEFAULT 'active',
		restart_count   INTEGER NOT NULL DEFAULT 0,
		exit_count      INTEGER NOT NULL DEFAULT 0,
		created_at      TEXT NOT NULL,
		last_started_at TEXT,
		last_exited_at  TEXT,
		error_message   TEXT
	)`)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func createTask(db *sql.DB, r TaskRecord) error {
	argsJSON, err := json.Marshal(r.Args)
	if err != nil {
		return err
	}
	var rpJSON *string
	if r.RetryPolicy != nil {
		b, err := json.Marshal(r.RetryPolicy)
		if err != nil {
			return err
		}
		s := string(b)
		rpJSON = &s
	}
	_, err = db.Exec(`INSERT INTO tasks
		(task_id, command, args, retry_policy, state, restart_count, exit_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TaskID, r.Command, string(argsJSON), rpJSON, r.State,
		r.RestartCount, r.ExitCount, r.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func getTask(db *sql.DB, taskID string) (*TaskRecord, error) {
	row := db.QueryRow(`SELECT task_id, command, args, retry_policy, state,
		restart_count, exit_count, created_at, last_started_at, last_exited_at, error_message
		FROM tasks WHERE task_id = ?`, taskID)
	return scanTaskRow(row)
}

func listTasks(db *sql.DB) ([]TaskRecord, error) {
	rows, err := db.Query(`SELECT task_id, command, args, retry_policy, state,
		restart_count, exit_count, created_at, last_started_at, last_exited_at, error_message
		FROM tasks ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRecord
	for rows.Next() {
		r, err := scanTaskRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func updateTaskState(db *sql.DB, taskID, state, errMsg string) error {
	_, err := db.Exec(`UPDATE tasks SET state = ?, error_message = ? WHERE task_id = ?`,
		state, errMsg, taskID)
	return err
}

func updateTaskStats(db *sql.DB, taskID string, restartCount, exitCount int, lastStartedAt, lastExitedAt *time.Time) error {
	var ls, le *string
	if lastStartedAt != nil {
		s := lastStartedAt.UTC().Format(time.RFC3339Nano)
		ls = &s
	}
	if lastExitedAt != nil {
		s := lastExitedAt.UTC().Format(time.RFC3339Nano)
		le = &s
	}
	_, err := db.Exec(`UPDATE tasks SET restart_count = ?, exit_count = ?, last_started_at = ?, last_exited_at = ? WHERE task_id = ?`,
		restartCount, exitCount, ls, le, taskID)
	return err
}

func deleteTask(db *sql.DB, taskID string) error {
	_, err := db.Exec(`DELETE FROM tasks WHERE task_id = ?`, taskID)
	return err
}

// scanner interface covers both *sql.Row and *sql.Rows
type scanner interface {
	Scan(dest ...any) error
}

func scanTaskRow(s scanner) (*TaskRecord, error) {
	var r TaskRecord
	var argsJSON string
	var rpJSON *string
	var createdAtStr string
	var lastStartedAtStr, lastExitedAtStr *string
	var errMsg *string

	err := s.Scan(&r.TaskID, &r.Command, &argsJSON, &rpJSON, &r.State,
		&r.RestartCount, &r.ExitCount, &createdAtStr, &lastStartedAtStr, &lastExitedAtStr, &errMsg)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(argsJSON), &r.Args); err != nil {
		r.Args = []string{}
	}
	if rpJSON != nil {
		r.RetryPolicy = &RetryPolicy{}
		_ = json.Unmarshal([]byte(*rpJSON), r.RetryPolicy)
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtStr)
	if lastStartedAtStr != nil {
		t, _ := time.Parse(time.RFC3339Nano, *lastStartedAtStr)
		r.LastStartedAt = &t
	}
	if lastExitedAtStr != nil {
		t, _ := time.Parse(time.RFC3339Nano, *lastExitedAtStr)
		r.LastExitedAt = &t
	}
	if errMsg != nil {
		r.ErrorMessage = *errMsg
	}
	return &r, nil
}

package overseer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is incremented whenever a breaking schema change is made.
// OpenDB returns ErrSchemaMismatch if the on-disk version is older.
const schemaVersion = 2

// ErrSchemaMismatch is returned by OpenDB when the database was created by an
// older (or newer) version of the library and needs migration.
var ErrSchemaMismatch = errors.New("overseer: database schema version mismatch — manual migration required")

type TaskRecord struct {
	TaskID         string
	Command        string
	Args           []string
	RetryPolicy    *RetryPolicy
	State          TaskState // see StateActive / StateStopped / StateErrored
	RestartCount   int
	ExitCount      int
	CreatedAt      time.Time
	LastStartedAt  *time.Time
	LastExitedAt   *time.Time
	ErrorMessage   string
	ExitTimestamps []time.Time // persisted exit times for error-window tracking
}

// OpenDB opens (or creates) the SQLite database at path and applies the schema.
// Pass ":memory:" for an in-memory database (useful in tests).
//
// WAL journal mode and a 5 s busy timeout are always enabled so that multiple
// processes sharing the same file do not deadlock under concurrent load.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Enable WAL mode and a busy timeout before touching any application tables.
	// These pragmas are idempotent and safe to run on every open.
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("overseer: failed to set pragma %q: %w", pragma, err)
		}
	}

	// Schema version table — created once, never mutated.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}

	var ver int
	err = db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&ver)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh database — write the current version.
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
			db.Close()
			return nil, err
		}
	case err != nil:
		db.Close()
		return nil, err
	case ver != schemaVersion:
		db.Close()
		return nil, fmt.Errorf("%w: on-disk version %d, library version %d", ErrSchemaMismatch, ver, schemaVersion)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		task_id          TEXT PRIMARY KEY,
		command          TEXT NOT NULL,
		args             TEXT NOT NULL,
		retry_policy     TEXT,
		state            TEXT NOT NULL DEFAULT 'active',
		restart_count    INTEGER NOT NULL DEFAULT 0,
		exit_count       INTEGER NOT NULL DEFAULT 0,
		created_at       TEXT NOT NULL,
		last_started_at  TEXT,
		last_exited_at   TEXT,
		error_message    TEXT,
		exit_timestamps  TEXT NOT NULL DEFAULT '[]'
	)`); err != nil {
		db.Close()
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
	tsJSON := encodeTimestamps(r.ExitTimestamps)
	_, err = db.Exec(`INSERT INTO tasks
		(task_id, command, args, retry_policy, state, restart_count, exit_count, created_at, exit_timestamps)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TaskID, r.Command, string(argsJSON), rpJSON, string(r.State),
		r.RestartCount, r.ExitCount, r.CreatedAt.UTC().Format(time.RFC3339Nano), tsJSON,
	)
	return err
}

func getTask(db *sql.DB, taskID string) (*TaskRecord, error) {
	row := db.QueryRow(`SELECT task_id, command, args, retry_policy, state,
		restart_count, exit_count, created_at, last_started_at, last_exited_at, error_message, exit_timestamps
		FROM tasks WHERE task_id = ?`, taskID)
	return scanTaskRow(row)
}

func listTasks(db *sql.DB) ([]TaskRecord, error) {
	rows, err := db.Query(`SELECT task_id, command, args, retry_policy, state,
		restart_count, exit_count, created_at, last_started_at, last_exited_at, error_message, exit_timestamps
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

func updateTaskState(db *sql.DB, taskID string, state TaskState, errMsg string) error {
	_, err := db.Exec(`UPDATE tasks SET state = ?, error_message = ? WHERE task_id = ?`,
		string(state), errMsg, taskID)
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

func updateTaskExitTimestamps(db *sql.DB, taskID string, ts []time.Time) error {
	_, err := db.Exec(`UPDATE tasks SET exit_timestamps = ? WHERE task_id = ?`,
		encodeTimestamps(ts), taskID)
	return err
}

func deleteTask(db *sql.DB, taskID string) error {
	_, err := db.Exec(`DELETE FROM tasks WHERE task_id = ?`, taskID)
	return err
}

// encodeTimestamps serialises a []time.Time as a JSON array of RFC3339Nano strings.
func encodeTimestamps(ts []time.Time) string {
	strs := make([]string, len(ts))
	for i, t := range ts {
		strs[i] = t.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(strs)
	return string(b)
}

// decodeTimestamps deserialises a JSON array of RFC3339Nano strings into []time.Time.
func decodeTimestamps(s string) []time.Time {
	var strs []string
	if err := json.Unmarshal([]byte(s), &strs); err != nil {
		return nil
	}
	ts := make([]time.Time, 0, len(strs))
	for _, str := range strs {
		t, err := time.Parse(time.RFC3339Nano, str)
		if err == nil {
			ts = append(ts, t)
		}
	}
	return ts
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
	var exitTsJSON string
	var stateStr string

	err := s.Scan(&r.TaskID, &r.Command, &argsJSON, &rpJSON, &stateStr,
		&r.RestartCount, &r.ExitCount, &createdAtStr, &lastStartedAtStr, &lastExitedAtStr, &errMsg, &exitTsJSON)
	if err != nil {
		return nil, err
	}

	r.State = TaskState(stateStr)
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
	r.ExitTimestamps = decodeTimestamps(exitTsJSON)
	return &r, nil
}

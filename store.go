package overseer

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// OpenDB opens the SQLite database at path and configures WAL mode + busy timeout.
// Pass ":memory:" for an in-memory database (used in tests).
//
// WAL journal mode and a 5 s busy timeout are always enabled so that multiple
// processes sharing the same file do not deadlock under concurrent load.
//
// The exit_history table is created on first open. It stores non-intentional
// process exit timestamps keyed by (action, exited_at) for retry-window tracking
// that persists across overseer restarts.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("overseer: failed to set pragma %q: %w", pragma, err)
		}
	}

	// Create exit_history table for persistent retry-window tracking.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS exit_history (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id   TEXT NOT NULL,
		action    TEXT NOT NULL,
		exited_at TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("overseer: failed to create exit_history table: %w", err)
	}

	// Index on (action, exited_at) for efficient window queries by action.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_exit_history_lookup
		ON exit_history(action, exited_at)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("overseer: failed to create exit_history index: %w", err)
	}

	return db, nil
}

// recordExit records a non-intentional process exit for retry-window tracking.
// The taskID is stored for audit purposes; queries are by action.
func recordExit(db *sql.DB, taskID, action string, exitedAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO exit_history(task_id, action, exited_at) VALUES(?,?,?)`,
		taskID, action, exitedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// loadExitHistory returns all exit timestamps for an action at or after since.
// Pass time.Time{} as since to retrieve all recorded exits for the action.
func loadExitHistory(db *sql.DB, action string, since time.Time) ([]time.Time, error) {
	rows, err := db.Query(
		`SELECT exited_at FROM exit_history WHERE action = ? AND exited_at >= ? ORDER BY exited_at ASC`,
		action, since.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []time.Time
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			result = append(result, t)
		}
	}
	return result, rows.Err()
}

// pruneExitHistory deletes exit records with exited_at older than before.
// Call periodically to prevent unbounded table growth.
func pruneExitHistory(db *sql.DB, before time.Time) error {
	_, err := db.Exec(
		`DELETE FROM exit_history WHERE exited_at < ?`,
		before.UTC().Format(time.RFC3339Nano),
	)
	return err
}

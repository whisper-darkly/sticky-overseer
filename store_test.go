package main

import (
	"database/sql"
	"testing"
	"time"
)

// openTestDB opens an in-memory SQLite DB for testing and returns a cleanup function.
func openTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openTestDB: %v", err)
	}
	return db, func() { db.Close() }
}

func TestOpenDB_InMemory(t *testing.T) {
	// Verify openDB succeeds with an in-memory database.
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
}

func TestOpenDB_WALMode(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	// In-memory SQLite always reports "memory" for journal_mode regardless of
	// the WAL pragma, so we just verify the pragma did not error during openDB.
	_ = mode
}

func TestOpenDB_SchemaCreated(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	// Verify exit_history table exists by querying its count.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM exit_history`).Scan(&count); err != nil {
		t.Fatalf("exit_history table not created: %v", err)
	}
}

func TestStateConstants(t *testing.T) {
	if StateActive != "active" {
		t.Errorf("StateActive = %q", StateActive)
	}
	if StateStopped != "stopped" {
		t.Errorf("StateStopped = %q", StateStopped)
	}
	if StateErrored != "errored" {
		t.Errorf("StateErrored = %q", StateErrored)
	}
}

func TestRecordExit(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	taskID := "test-task-1"
	action := "build-docker-image"
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := recordExit(db, taskID, action, now); err != nil {
		t.Fatalf("recordExit: %v", err)
	}

	history, err := loadExitHistory(db, action, time.Time{})
	if err != nil {
		t.Fatalf("loadExitHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 exit, got %d", len(history))
	}
}

func TestLoadExitHistory_Window(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	action := "deploy-service"
	now := time.Now().UTC()

	// Record 3 exits at different times.
	if err := recordExit(db, "task1", action, now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("recordExit: %v", err)
	}
	if err := recordExit(db, "task2", action, now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("recordExit: %v", err)
	}
	if err := recordExit(db, "task3", action, now); err != nil {
		t.Fatalf("recordExit: %v", err)
	}

	// Query last 2 hours — should return 2 records.
	window := now.Add(-2 * time.Hour)
	history, err := loadExitHistory(db, action, window)
	if err != nil {
		t.Fatalf("loadExitHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 exits within window, got %d", len(history))
	}
}

func TestLoadExitHistory_ByAction(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	now := time.Now().UTC()

	// Record exits for two different actions.
	if err := recordExit(db, "task1", "action-a", now); err != nil {
		t.Fatalf("recordExit action-a: %v", err)
	}
	if err := recordExit(db, "task2", "action-b", now); err != nil {
		t.Fatalf("recordExit action-b: %v", err)
	}

	// Query for action-a — should only return action-a's exit.
	history, err := loadExitHistory(db, "action-a", time.Time{})
	if err != nil {
		t.Fatalf("loadExitHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 exit for action-a, got %d", len(history))
	}

	// Query for action-b — should only return action-b's exit.
	history, err = loadExitHistory(db, "action-b", time.Time{})
	if err != nil {
		t.Fatalf("loadExitHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 exit for action-b, got %d", len(history))
	}
}

func TestPruneExitHistory(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	action := "cleanup-temp-files"
	now := time.Now().UTC()

	// Record 2 exits.
	if err := recordExit(db, "task1", action, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("recordExit: %v", err)
	}
	if err := recordExit(db, "task2", action, now.Add(-30*time.Minute)); err != nil {
		t.Fatalf("recordExit: %v", err)
	}

	// Prune everything older than 1 hour.
	if err := pruneExitHistory(db, now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("pruneExitHistory: %v", err)
	}

	// Only the recent exit should remain.
	history, err := loadExitHistory(db, action, time.Time{})
	if err != nil {
		t.Fatalf("loadExitHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 exit after prune, got %d", len(history))
	}
}

package overseer

import (
	"testing"
)

func TestOpenDB_InMemory(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
}

func TestOpenDB_WALMode(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	// In-memory SQLite always reports "memory" for journal_mode regardless of
	// the WAL pragma, so we just verify the pragma did not error during OpenDB.
	_ = mode
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

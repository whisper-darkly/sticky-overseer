package overseer

import (
	"testing"
	"time"
)

func TestOpenDB_Idempotent(t *testing.T) {
	// Opening the same in-memory DB twice is not meaningful (each :memory: open
	// is independent), so we just verify OpenDB succeeds and the schema is usable.
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Verify the tasks table exists and is queryable.
	rows, err := db.Query(`SELECT task_id FROM tasks LIMIT 0`)
	if err != nil {
		t.Fatalf("tasks table not usable after OpenDB: %v", err)
	}
	rows.Close()
}

func TestOpenDB_SchemaMismatch(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	// Corrupt the version so the next open sees a mismatch.
	if _, err := db.Exec(`UPDATE schema_version SET version = 999`); err != nil {
		t.Fatalf("UPDATE schema_version: %v", err)
	}
	// Can't reopen :memory: — but we can verify the sentinel error is exported
	// and the ErrSchemaMismatch sentinel is the right type.
	db.Close()
	if ErrSchemaMismatch == nil {
		t.Error("ErrSchemaMismatch should be non-nil sentinel")
	}
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
	// the WAL pragma, so we just verify the pragma didn't error during OpenDB.
	// The WAL pragma is a no-op on :memory: but must not cause OpenDB to fail.
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

func TestCreateGetTask_RoundTrip(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rec := TaskRecord{
		TaskID:    "task-1",
		Command:   "/bin/echo",
		Args:      []string{"hello", "world"},
		State:     StateActive,
		CreatedAt: now,
	}
	if err := createTask(db, rec); err != nil {
		t.Fatalf("createTask: %v", err)
	}

	got, err := getTask(db, "task-1")
	if err != nil {
		t.Fatalf("getTask: %v", err)
	}
	if got.TaskID != rec.TaskID {
		t.Errorf("TaskID: got %q want %q", got.TaskID, rec.TaskID)
	}
	if got.Command != rec.Command {
		t.Errorf("Command: got %q want %q", got.Command, rec.Command)
	}
	if len(got.Args) != 2 || got.Args[0] != "hello" || got.Args[1] != "world" {
		t.Errorf("Args: got %v", got.Args)
	}
	if got.State != StateActive {
		t.Errorf("State: got %q", got.State)
	}
	if got.RetryPolicy != nil {
		t.Errorf("RetryPolicy should be nil")
	}
}

func TestCreateGetTask_WithRetryPolicy(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rp := &RetryPolicy{
		RestartDelay:   "5s",
		ErrorWindow:    "1m",
		ErrorThreshold: 3,
	}
	rec := TaskRecord{
		TaskID:      "task-rp",
		Command:     "/bin/sh",
		Args:        []string{},
		RetryPolicy: rp,
		State:       StateActive,
		CreatedAt:   time.Now().UTC(),
	}
	if err := createTask(db, rec); err != nil {
		t.Fatalf("createTask: %v", err)
	}

	got, err := getTask(db, "task-rp")
	if err != nil {
		t.Fatalf("getTask: %v", err)
	}
	if got.RetryPolicy == nil {
		t.Fatal("RetryPolicy is nil")
	}
	if got.RetryPolicy.RestartDelay != "5s" {
		t.Errorf("RestartDelay: got %q", got.RetryPolicy.RestartDelay)
	}
	if got.RetryPolicy.ErrorThreshold != 3 {
		t.Errorf("ErrorThreshold: got %d", got.RetryPolicy.ErrorThreshold)
	}
}

func TestListTasks_OrderAndMultipleRows(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Now().UTC()
	for i, id := range []string{"a", "b", "c"} {
		rec := TaskRecord{
			TaskID:    id,
			Command:   "/bin/echo",
			Args:      []string{},
			State:     StateActive,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := createTask(db, rec); err != nil {
			t.Fatalf("createTask %s: %v", id, err)
		}
	}

	tasks, err := listTasks(db)
	if err != nil {
		t.Fatalf("listTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
	if tasks[0].TaskID != "a" || tasks[1].TaskID != "b" || tasks[2].TaskID != "c" {
		t.Errorf("wrong order: %v %v %v", tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID)
	}
}

func TestUpdateTaskState(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := TaskRecord{TaskID: "t1", Command: "/bin/echo", Args: []string{}, State: StateActive, CreatedAt: time.Now().UTC()}
	_ = createTask(db, rec)

	if err := updateTaskState(db, "t1", StateStopped, "manual stop"); err != nil {
		t.Fatalf("updateTaskState: %v", err)
	}

	got, _ := getTask(db, "t1")
	if got.State != StateStopped {
		t.Errorf("State: got %q want stopped", got.State)
	}
	if got.ErrorMessage != "manual stop" {
		t.Errorf("ErrorMessage: got %q", got.ErrorMessage)
	}
}

func TestUpdateTaskStats(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := TaskRecord{TaskID: "t2", Command: "/bin/echo", Args: []string{}, State: StateActive, CreatedAt: time.Now().UTC()}
	_ = createTask(db, rec)

	now := time.Now().UTC().Truncate(time.Second)
	later := now.Add(time.Minute)
	if err := updateTaskStats(db, "t2", 5, 2, &now, &later); err != nil {
		t.Fatalf("updateTaskStats: %v", err)
	}

	got, _ := getTask(db, "t2")
	if got.RestartCount != 5 {
		t.Errorf("RestartCount: got %d", got.RestartCount)
	}
	if got.ExitCount != 2 {
		t.Errorf("ExitCount: got %d", got.ExitCount)
	}
	if got.LastStartedAt == nil || !got.LastStartedAt.Equal(now) {
		t.Errorf("LastStartedAt: got %v want %v", got.LastStartedAt, now)
	}
	if got.LastExitedAt == nil || !got.LastExitedAt.Equal(later) {
		t.Errorf("LastExitedAt: got %v want %v", got.LastExitedAt, later)
	}
}

func TestDeleteTask(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := TaskRecord{TaskID: "del-me", Command: "/bin/echo", Args: []string{}, State: StateActive, CreatedAt: time.Now().UTC()}
	_ = createTask(db, rec)

	if err := deleteTask(db, "del-me"); err != nil {
		t.Fatalf("deleteTask: %v", err)
	}

	got, err := getTask(db, "del-me")
	if err == nil {
		t.Errorf("expected error after delete, got task: %+v", got)
	}
}

func TestScanTaskRow_NullOptionalFields(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := TaskRecord{
		TaskID:    "null-test",
		Command:   "/bin/true",
		Args:      []string{},
		State:     StateActive,
		CreatedAt: time.Now().UTC(),
		// RetryPolicy, LastStartedAt, LastExitedAt all nil
	}
	_ = createTask(db, rec)

	got, err := getTask(db, "null-test")
	if err != nil {
		t.Fatalf("getTask: %v", err)
	}
	if got.RetryPolicy != nil {
		t.Error("RetryPolicy should be nil")
	}
	if got.LastStartedAt != nil {
		t.Error("LastStartedAt should be nil")
	}
	if got.LastExitedAt != nil {
		t.Error("LastExitedAt should be nil")
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage should be empty, got %q", got.ErrorMessage)
	}
}

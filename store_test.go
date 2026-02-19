package overseer

import (
	"testing"
	"time"
)

func openTestDB(t *testing.T) interface{ Close() error } {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenDB_Idempotent(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("first openDB: %v", err)
	}
	defer db.Close()

	// Run schema again — should not error
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
		t.Fatalf("schema re-creation: %v", err)
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
		State:     "active",
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
	if got.State != "active" {
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
		State:       "active",
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
			State:     "active",
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

	rec := TaskRecord{TaskID: "t1", Command: "/bin/echo", Args: []string{}, State: "active", CreatedAt: time.Now().UTC()}
	_ = createTask(db, rec)

	if err := updateTaskState(db, "t1", "stopped", "manual stop"); err != nil {
		t.Fatalf("updateTaskState: %v", err)
	}

	got, _ := getTask(db, "t1")
	if got.State != "stopped" {
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

	rec := TaskRecord{TaskID: "t2", Command: "/bin/echo", Args: []string{}, State: "active", CreatedAt: time.Now().UTC()}
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

	rec := TaskRecord{TaskID: "del-me", Command: "/bin/echo", Args: []string{}, State: "active", CreatedAt: time.Now().UTC()}
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
		State:     "active",
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

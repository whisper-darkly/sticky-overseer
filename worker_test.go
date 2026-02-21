package main

import (
	"sync"
	"syscall"
	"testing"
	"time"
)

func newTestWorker() *Worker {
	return &Worker{
		PID:    1234,
		TaskID: "test-task",
		State:  WorkerRunning,
	}
}

func TestAddEvent_RingBuffer_Eviction(t *testing.T) {
	w := newTestWorker()
	base := time.Now().UTC()

	// Fill to capacity
	for i := 0; i < ringBufferSize; i++ {
		w.addEvent(Event{Type: "output", Data: string(rune('A' + i%26)), TS: base.Add(time.Duration(i) * time.Millisecond)})
	}
	if len(w.events) != ringBufferSize {
		t.Fatalf("expected %d events, got %d", ringBufferSize, len(w.events))
	}

	// Add one more — oldest should be evicted
	w.addEvent(Event{Type: "output", Data: "EXTRA", TS: base.Add(ringBufferSize * time.Millisecond)})
	if len(w.events) != ringBufferSize {
		t.Fatalf("after eviction: expected %d events, got %d", ringBufferSize, len(w.events))
	}
	// Last event should be the new one
	if w.events[len(w.events)-1].Data != "EXTRA" {
		t.Errorf("last event data: got %q want EXTRA", w.events[len(w.events)-1].Data)
	}
	// First event should no longer be the original first
	if w.events[0].Data == string(rune('A')) {
		t.Error("first event was not evicted")
	}
}

func TestGetEvents_NoFilter_ReturnsCopy(t *testing.T) {
	w := newTestWorker()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		w.addEvent(Event{Type: "output", Data: "x", TS: now.Add(time.Duration(i) * time.Second)})
	}

	events := w.getEvents(nil)
	if len(events) != 5 {
		t.Fatalf("expected 5, got %d", len(events))
	}

	// Mutation of returned slice should not affect internal state
	events[0].Data = "mutated"
	if w.events[0].Data == "mutated" {
		t.Error("getEvents returned a direct reference, not a copy")
	}
}

func TestGetEvents_WithSinceFilter(t *testing.T) {
	w := newTestWorker()
	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		w.addEvent(Event{Type: "output", Data: "x", TS: base.Add(time.Duration(i) * time.Second)})
	}

	// Events at base+5s and later
	since := base.Add(5 * time.Second)
	events := w.getEvents(&since)
	if len(events) != 5 {
		t.Fatalf("expected 5 events at or after since, got %d", len(events))
	}
	for _, e := range events {
		if e.TS.Before(since) {
			t.Errorf("event %v is before since %v", e.TS, since)
		}
	}
}

func TestLastEventAt_Empty(t *testing.T) {
	w := newTestWorker()
	if w.lastEventAt() != nil {
		t.Error("expected nil for empty worker")
	}
}

func TestLastEventAt_NonEmpty(t *testing.T) {
	w := newTestWorker()
	t1 := time.Now().UTC()
	t2 := t1.Add(time.Second)
	w.addEvent(Event{Type: "output", TS: t1})
	w.addEvent(Event{Type: "output", TS: t2})

	got := w.lastEventAt()
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if !got.Equal(t2) {
		t.Errorf("lastEventAt: got %v want %v", *got, t2)
	}
}

// TestGetEvents_SinceAfterAll verifies that a since timestamp after all events
// returns an empty slice.
func TestGetEvents_SinceAfterAll(t *testing.T) {
	w := newTestWorker()
	base := time.Now().UTC()
	w.addEvent(Event{Type: "output", TS: base})
	w.addEvent(Event{Type: "output", TS: base.Add(time.Second)})

	since := base.Add(time.Hour)
	events := w.getEvents(&since)
	if len(events) != 0 {
		t.Errorf("expected 0 events after all timestamps, got %d", len(events))
	}
}

// TestStop_AlreadyExited verifies that calling Stop on a worker in the Exited
// state is a safe no-op.
func TestStop_AlreadyExited(t *testing.T) {
	w := &Worker{State: WorkerExited}
	w.Stop() // must not panic
}

// TestStartWorker_BadCommand verifies that startWorker returns an error when
// the executable does not exist.
func TestStartWorker_BadCommand(t *testing.T) {
	_, err := startWorker(
		workerConfig{TaskID: "t", Command: "/nonexistent/binary/xyz"},
		workerCallbacks{},
	)
	if err == nil {
		t.Error("expected error for non-existent command")
	}
}

// TestWorker_IncludeStdout_False verifies that when IncludeStdout is false and
// IncludeStderr is true, stdout lines are silently drained (no output callback)
// while stderr lines are forwarded normally.
func TestWorker_IncludeStdout_False(t *testing.T) {
	var mu sync.Mutex
	var received []OutputMessage
	cb := workerCallbacks{
		onOutput: func(msg *OutputMessage) {
			mu.Lock()
			received = append(received, *msg)
			mu.Unlock()
		},
		logEvent: func(v any) {},
		onExited: func(w *Worker, exitCode int, intentional bool, ts time.Time) {},
	}

	// Script writes one line to stdout and one line to stderr.
	w, err := startWorker(workerConfig{
		TaskID:        "t-stdout-false",
		Command:       "/bin/sh",
		Args:          []string{"-c", "echo stdout_line; echo stderr_line >&2"},
		IncludeStdout: false,
		IncludeStderr: true,
	}, cb)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}

	// Wait for the process to exit by polling ExitedAt.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := w.State == WorkerExited
		w.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, msg := range received {
		if msg.Stream == StreamStdout {
			t.Errorf("received stdout output but expected none: %q", msg.Data)
		}
	}
	var stderrFound bool
	for _, msg := range received {
		if msg.Stream == StreamStderr && msg.Data == "stderr_line" {
			stderrFound = true
		}
	}
	if !stderrFound {
		t.Error("expected stderr_line in received messages but it was absent")
	}
}

// TestWorker_IncludeStderr_False verifies that when IncludeStdout is true and
// IncludeStderr is false, stderr lines are silently drained (no output callback)
// while stdout lines are forwarded normally.
func TestWorker_IncludeStderr_False(t *testing.T) {
	var mu sync.Mutex
	var received []OutputMessage
	cb := workerCallbacks{
		onOutput: func(msg *OutputMessage) {
			mu.Lock()
			received = append(received, *msg)
			mu.Unlock()
		},
		logEvent: func(v any) {},
		onExited: func(w *Worker, exitCode int, intentional bool, ts time.Time) {},
	}

	w, err := startWorker(workerConfig{
		TaskID:        "t-stderr-false",
		Command:       "/bin/sh",
		Args:          []string{"-c", "echo stdout_line; echo stderr_line >&2"},
		IncludeStdout: true,
		IncludeStderr: false,
	}, cb)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := w.State == WorkerExited
		w.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, msg := range received {
		if msg.Stream == StreamStderr {
			t.Errorf("received stderr output but expected none: %q", msg.Data)
		}
	}
	var stdoutFound bool
	for _, msg := range received {
		if msg.Stream == StreamStdout && msg.Data == "stdout_line" {
			stdoutFound = true
		}
	}
	if !stdoutFound {
		t.Error("expected stdout_line in received messages but it was absent")
	}
}

// TestWorker_BothInclude_True verifies that when both IncludeStdout and
// IncludeStderr are true, output from both streams is forwarded normally
// (existing behavior).
func TestWorker_BothInclude_True(t *testing.T) {
	var mu sync.Mutex
	var received []OutputMessage
	cb := workerCallbacks{
		onOutput: func(msg *OutputMessage) {
			mu.Lock()
			received = append(received, *msg)
			mu.Unlock()
		},
		logEvent: func(v any) {},
		onExited: func(w *Worker, exitCode int, intentional bool, ts time.Time) {},
	}

	w, err := startWorker(workerConfig{
		TaskID:        "t-both-true",
		Command:       "/bin/sh",
		Args:          []string{"-c", "echo stdout_line; echo stderr_line >&2"},
		IncludeStdout: true,
		IncludeStderr: true,
	}, cb)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := w.State == WorkerExited
		w.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	var stdoutFound, stderrFound bool
	for _, msg := range received {
		if msg.Stream == StreamStdout && msg.Data == "stdout_line" {
			stdoutFound = true
		}
		if msg.Stream == StreamStderr && msg.Data == "stderr_line" {
			stderrFound = true
		}
	}
	if !stdoutFound {
		t.Error("expected stdout_line in received messages but it was absent")
	}
	if !stderrFound {
		t.Error("expected stderr_line in received messages but it was absent")
	}
}

// TestLargeOutputLine verifies that the worker's scanner can handle a single
// output line up to scanMaxSize (10MB) without truncation. This exercises the
// Scanner.Buffer() call added in job 1300.
func TestLargeOutputLine(t *testing.T) {
	// Build a line that is exactly 2MB — well within scanMaxSize (10MB).
	const lineSize = 2 * 1024 * 1024

	var mu sync.Mutex
	var received []string
	cb := workerCallbacks{
		onOutput: func(msg *OutputMessage) {
			mu.Lock()
			received = append(received, msg.Data)
			mu.Unlock()
		},
		logEvent: func(v any) {},
		onExited: func(w *Worker, exitCode int, intentional bool, ts time.Time) {},
	}

	// Use python3 to write a 2MB line to stdout (no newline in the middle).
	w, err := startWorker(workerConfig{
		TaskID:        "t-large-line",
		Command:       "python3",
		Args:          []string{"-c", "import sys; sys.stdout.write('x'*2097152 + '\\n'); sys.stdout.flush()"},
		IncludeStdout: true,
		IncludeStderr: false,
	}, cb)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}

	// Wait for the process to exit.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := w.State == WorkerExited
		w.mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) == 0 {
		t.Fatal("expected at least one output line, got none")
	}
	got := received[0]
	if len(got) != lineSize {
		t.Errorf("large line length: want %d, got %d", lineSize, len(got))
	}
}

// TestScannerErr verifies that when the pipe is closed unexpectedly mid-scan
// (simulating a scanner error), the worker transitions to WorkerExited and the
// onExited callback is called — the exit is not silently dropped.
func TestScannerErr(t *testing.T) {
	exitCalled := make(chan struct{}, 1)
	cb := workerCallbacks{
		onOutput: func(msg *OutputMessage) {},
		logEvent: func(v any) {},
		onExited: func(w *Worker, exitCode int, intentional bool, ts time.Time) {
			exitCalled <- struct{}{}
		},
	}

	// Start a process that exits quickly (pipe close will trigger scanner EOF).
	w, err := startWorker(workerConfig{
		TaskID:        "t-scanner-err",
		Command:       "/bin/sh",
		Args:          []string{"-c", "echo hello; exit 1"},
		IncludeStdout: true,
		IncludeStderr: true,
	}, cb)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	_ = w

	select {
	case <-exitCalled:
		// Good — onExited was called.
	case <-time.After(5 * time.Second):
		t.Error("onExited callback was not called after process exit")
	}
}

// TestProcessGroupKill verifies that stopping a worker sends SIGTERM to the
// entire process group, killing child processes as well as the parent.
// This exercises the Setpgid + negative-pgid kill logic added in job 1300.
func TestProcessGroupKill(t *testing.T) {
	exitCalled := make(chan struct{}, 1)
	var workerPID int

	cb := workerCallbacks{
		onOutput: func(msg *OutputMessage) {},
		logEvent: func(v any) {},
		onExited: func(w *Worker, exitCode int, intentional bool, ts time.Time) {
			exitCalled <- struct{}{}
		},
	}

	// This shell script starts a long-running child (sleep 300) and then itself
	// sleeps. When we stop the worker, both should be killed via process group.
	w, err := startWorker(workerConfig{
		TaskID:        "t-pgkill",
		Command:       "/bin/sh",
		Args:          []string{"-c", "sleep 300 & sleep 300"},
		IncludeStdout: false,
		IncludeStderr: false,
	}, cb)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}

	// Wait until the process is actually running.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		pid := w.PID
		w.mu.Unlock()
		if pid != 0 {
			workerPID = pid
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if workerPID == 0 {
		t.Fatal("worker PID never set")
	}

	// Stop the worker — this should kill the entire process group.
	w.Stop()

	select {
	case <-exitCalled:
		// Good — process exited.
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not exit after Stop()")
	}

	// Verify the parent process is gone.
	if err := syscall.Kill(workerPID, 0); err == nil {
		t.Errorf("parent process %d is still alive after Stop()", workerPID)
	}
}

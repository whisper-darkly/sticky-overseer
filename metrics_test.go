package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestMetrics_RecordStart(t *testing.T) {
	m := NewMetrics()
	m.RecordStart("task1", "build")
	m.RecordStart("task2", "build")
	m.RecordStart("task3", "deploy")

	g := m.GlobalSnapshot()
	if g.TasksStarted != 3 {
		t.Errorf("TasksStarted: want 3, got %d", g.TasksStarted)
	}

	a := m.ActionSnapshot("build")
	if a == nil {
		t.Fatal("ActionSnapshot(build) returned nil")
	}
	if a.TasksStarted != 2 {
		t.Errorf("build TasksStarted: want 2, got %d", a.TasksStarted)
	}

	d := m.ActionSnapshot("deploy")
	if d == nil {
		t.Fatal("ActionSnapshot(deploy) returned nil")
	}
	if d.TasksStarted != 1 {
		t.Errorf("deploy TasksStarted: want 1, got %d", d.TasksStarted)
	}

	// Per-task entry should be initialised.
	te := m.TaskSnapshot("task1")
	if te == nil {
		t.Fatal("TaskSnapshot(task1) returned nil after RecordStart")
	}
}

func TestMetrics_RecordExit(t *testing.T) {
	m := NewMetrics()
	m.RecordStart("t1", "build")
	m.RecordExit("t1", "build", 0, false, 500)

	g := m.GlobalSnapshot()
	if g.TasksCompleted != 1 {
		t.Errorf("TasksCompleted: want 1, got %d", g.TasksCompleted)
	}
	if g.TasksErrored != 0 {
		t.Errorf("TasksErrored: want 0, got %d", g.TasksErrored)
	}

	te := m.TaskSnapshot("t1")
	if te == nil {
		t.Fatal("TaskSnapshot(t1) nil")
	}
	if te.RuntimeMs != 500 {
		t.Errorf("RuntimeMs: want 500, got %d", te.RuntimeMs)
	}
	if te.LastExitCode != 0 {
		t.Errorf("LastExitCode: want 0, got %d", te.LastExitCode)
	}
}

func TestMetrics_RecordExit_Errored(t *testing.T) {
	m := NewMetrics()
	m.RecordStart("t1", "build")
	m.RecordExit("t1", "build", 1, true, 100)

	g := m.GlobalSnapshot()
	if g.TasksErrored != 1 {
		t.Errorf("TasksErrored: want 1, got %d", g.TasksErrored)
	}

	a := m.ActionSnapshot("build")
	if a.TasksErrored != 1 {
		t.Errorf("build TasksErrored: want 1, got %d", a.TasksErrored)
	}

	te := m.TaskSnapshot("t1")
	if te.LastExitCode != 1 {
		t.Errorf("LastExitCode: want 1, got %d", te.LastExitCode)
	}
}

func TestMetrics_RecordRestart(t *testing.T) {
	m := NewMetrics()
	m.RecordStart("t1", "build")
	m.RecordRestart("t1", "build")
	m.RecordRestart("t1", "build")

	g := m.GlobalSnapshot()
	if g.TasksRestarted != 2 {
		t.Errorf("TasksRestarted: want 2, got %d", g.TasksRestarted)
	}

	a := m.ActionSnapshot("build")
	if a.TasksRestarted != 2 {
		t.Errorf("build TasksRestarted: want 2, got %d", a.TasksRestarted)
	}

	te := m.TaskSnapshot("t1")
	if te.RestartCount != 2 {
		t.Errorf("RestartCount: want 2, got %d", te.RestartCount)
	}
}

func TestMetrics_RecordOutput(t *testing.T) {
	m := NewMetrics()
	m.RecordStart("t1", "build")
	for i := 0; i < 10; i++ {
		m.RecordOutput("t1", "build")
	}

	g := m.GlobalSnapshot()
	if g.TotalOutputLines != 10 {
		t.Errorf("TotalOutputLines: want 10, got %d", g.TotalOutputLines)
	}

	a := m.ActionSnapshot("build")
	if a.TotalOutputLines != 10 {
		t.Errorf("build TotalOutputLines: want 10, got %d", a.TotalOutputLines)
	}

	te := m.TaskSnapshot("t1")
	if te.OutputLines != 10 {
		t.Errorf("OutputLines: want 10, got %d", te.OutputLines)
	}
}

func TestMetrics_QueueCounters(t *testing.T) {
	m := NewMetrics()
	m.RecordEnqueued()
	m.RecordEnqueued()
	m.RecordEnqueued()
	m.RecordDequeued()
	m.RecordDisplaced()
	m.RecordExpired()

	g := m.GlobalSnapshot()
	if g.Enqueued != 3 {
		t.Errorf("Enqueued: want 3, got %d", g.Enqueued)
	}
	if g.Dequeued != 1 {
		t.Errorf("Dequeued: want 1, got %d", g.Dequeued)
	}
	if g.Displaced != 1 {
		t.Errorf("Displaced: want 1, got %d", g.Displaced)
	}
	if g.Expired != 1 {
		t.Errorf("Expired: want 1, got %d", g.Expired)
	}
}

func TestMetrics_ActionSnapshot_Unknown(t *testing.T) {
	m := NewMetrics()
	if m.ActionSnapshot("nonexistent") != nil {
		t.Error("expected nil for unknown action")
	}
}

func TestMetrics_TaskSnapshot_Unknown(t *testing.T) {
	m := NewMetrics()
	if m.TaskSnapshot("nonexistent") != nil {
		t.Error("expected nil for unknown task")
	}
}

func TestMetrics_PerTaskEviction(t *testing.T) {
	m := NewMetrics()
	// Insert maxTaskEntries + 10 tasks.
	total := maxTaskEntries + 10
	for i := 0; i < total; i++ {
		taskID := fmt.Sprintf("task-%d", i)
		m.RecordStart(taskID, "build")
	}

	m.mu.RLock()
	count := len(m.byTask)
	m.mu.RUnlock()

	if count > maxTaskEntries {
		t.Errorf("byTask has %d entries, expected at most %d", count, maxTaskEntries)
	}
}

func TestMetrics_ConcurrentWrites(t *testing.T) {
	m := NewMetrics()
	const goroutines = 50
	const ops = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-%d", id)
			m.RecordStart(taskID, "build")
			for i := 0; i < ops; i++ {
				m.RecordOutput(taskID, "build")
			}
			m.RecordExit(taskID, "build", 0, false, 100)
		}(g)
	}
	wg.Wait()

	g := m.GlobalSnapshot()
	if g.TasksStarted != goroutines {
		t.Errorf("TasksStarted: want %d, got %d", goroutines, g.TasksStarted)
	}
	if g.TotalOutputLines != goroutines*ops {
		t.Errorf("TotalOutputLines: want %d, got %d", goroutines*ops, g.TotalOutputLines)
	}
}

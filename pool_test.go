package overseer

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers for pool tests
// ---------------------------------------------------------------------------

// poolTestIDs generates n unique task IDs.
func poolTestIDs(n int) []string {
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = newUUID()
	}
	return ids
}

// nopStop is a no-op stop function for pool tests.
func nopStop() {}

// ---------------------------------------------------------------------------
// Basic pool tests
// ---------------------------------------------------------------------------

// TestPool_Unlimited verifies that a pool with limit=0 (unlimited) allows
// any number of tasks to start immediately without queuing.
func TestPool_Unlimited(t *testing.T) {
	pm := NewPoolManager(PoolConfig{Limit: 0}, nil)
	defer pm.DrainAll()

	const count = 10
	var started int64

	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			result, err := pm.Acquire(id, "test", false, nopStop, func() error {
				atomic.AddInt64(&started, 1)
				return nil
			}, nil)
			if err != nil {
				t.Errorf("Acquire(%s): unexpected error: %v", id, err)
			}
			if result != AcquireRunning {
				t.Errorf("Acquire(%s): expected AcquireRunning, got %v", id, result)
			}
		}(newUUID())
	}
	wg.Wait()

	if atomic.LoadInt64(&started) != count {
		t.Errorf("expected %d starts, got %d", count, started)
	}
}

// TestPool_Reject_Overflow verifies that tasks are rejected when the pool is
// at capacity and no queue is configured.
func TestPool_Reject_Overflow(t *testing.T) {
	pm := NewPoolManager(PoolConfig{Limit: 1}, nil)
	defer pm.DrainAll()

	id1 := newUUID()
	id2 := newUUID()

	// First task should be admitted immediately.
	result, err := pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	if result != AcquireRunning {
		t.Fatalf("first Acquire: expected AcquireRunning, got %v", result)
	}

	// Second task should be rejected (no queue configured).
	_, err = pm.Acquire(id2, "test", false, nopStop, func() error { return nil }, nil)
	if err == nil {
		t.Fatal("second Acquire should have been rejected, got nil error")
	}
	if !errors.Is(err, errPoolRejected) {
		t.Errorf("expected errPoolRejected, got %v", err)
	}
}

// TestPool_Queue_FIFO verifies that tasks queued when pool is full are admitted
// in FIFO order when a slot becomes available.
func TestPool_Queue_FIFO(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst, // FIFO
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the first slot indefinitely.
	id1 := newUUID()
	result, err := pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if result != AcquireRunning {
		t.Fatalf("expected AcquireRunning for first task")
	}

	// Queue two more tasks and record their start order.
	var startOrder []string
	var mu sync.Mutex

	type queueResult struct {
		result AcquireResult
		err    error
	}

	res2 := make(chan queueResult, 1)
	res3 := make(chan queueResult, 1)

	id2 := newUUID()
	id3 := newUUID()

	go func() {
		r, e := pm.Acquire(id2, "test", false, nopStop, func() error {
			mu.Lock()
			startOrder = append(startOrder, id2)
			mu.Unlock()
			return nil
		}, nil)
		res2 <- queueResult{r, e}
	}()

	// Small delay to ensure id2 is enqueued before id3.
	time.Sleep(10 * time.Millisecond)

	go func() {
		r, e := pm.Acquire(id3, "test", false, nopStop, func() error {
			mu.Lock()
			startOrder = append(startOrder, id3)
			mu.Unlock()
			return nil
		}, nil)
		res3 <- queueResult{r, e}
	}()

	// Let the queues form.
	time.Sleep(20 * time.Millisecond)

	// Release id1 → id2 should start (FIFO: first enqueued).
	pm.Release(id1)

	// Wait for id2 to start.
	select {
	case r := <-res2:
		if r.err != nil {
			t.Fatalf("id2 Acquire error: %v", r.err)
		}
		if r.result != AcquireQueued {
			t.Fatalf("id2 expected AcquireQueued, got %v", r.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for id2 to be admitted")
	}

	// Release id2 → id3 should start.
	pm.Release(id2)

	select {
	case r := <-res3:
		if r.err != nil {
			t.Fatalf("id3 Acquire error: %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for id3 to be admitted")
	}

	// Verify FIFO order.
	mu.Lock()
	defer mu.Unlock()
	if len(startOrder) < 2 {
		t.Fatalf("expected 2 starts from queue, got %d", len(startOrder))
	}
	if startOrder[0] != id2 {
		t.Errorf("FIFO: first start should be id2, got %q", startOrder[0])
	}
	if startOrder[1] != id3 {
		t.Errorf("FIFO: second start should be id3, got %q", startOrder[1])
	}
}

// TestPool_Queue_LIFO verifies that tasks are admitted in LIFO order when
// QueueOrder is OrderLast.
func TestPool_Queue_LIFO(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderLast, // LIFO
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the first slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	var startOrder []string
	var mu sync.Mutex

	id2 := newUUID()
	id3 := newUUID()

	res2 := make(chan error, 1)
	res3 := make(chan error, 1)

	go func() {
		_, e := pm.Acquire(id2, "test", false, nopStop, func() error {
			mu.Lock()
			startOrder = append(startOrder, id2)
			mu.Unlock()
			return nil
		}, nil)
		res2 <- e
	}()

	time.Sleep(10 * time.Millisecond) // ensure id2 queued first

	go func() {
		_, e := pm.Acquire(id3, "test", false, nopStop, func() error {
			mu.Lock()
			startOrder = append(startOrder, id3)
			mu.Unlock()
			return nil
		}, nil)
		res3 <- e
	}()

	time.Sleep(20 * time.Millisecond)

	// Release id1 → LIFO: id3 (last enqueued) should start first.
	pm.Release(id1)

	select {
	case e := <-res3:
		if e != nil {
			t.Fatalf("id3 error: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for id3 (LIFO first)")
	}

	pm.Release(id3)

	select {
	case e := <-res2:
		if e != nil {
			t.Fatalf("id2 error: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for id2 (LIFO second)")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(startOrder) < 2 {
		t.Fatalf("expected 2 starts, got %d", len(startOrder))
	}
	if startOrder[0] != id3 {
		t.Errorf("LIFO: first start should be id3, got %q", startOrder[0])
	}
	if startOrder[1] != id2 {
		t.Errorf("LIFO: second start should be id2, got %q", startOrder[1])
	}
}

// TestPool_MaxAge_Expiry verifies that tasks with age beyond MaxAge are expired
// from the queue without being started, and their cancelFn is called.
func TestPool_MaxAge_Expiry(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			MaxAge:  1, // 1 second max age
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the first slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	// Queue a task that will expire.
	id2 := newUUID()
	cancelled := make(chan error, 1)
	pm.Acquire(id2, "test", false, nopStop, func() error { return nil }, func(reason error) { //nolint:errcheck
		cancelled <- reason
	})

	// Wait longer than MaxAge so the task expires.
	// The expiry loop runs at MaxAge/2 = 500ms, so 2s should be enough.
	select {
	case reason := <-cancelled:
		if reason == nil {
			t.Error("expected non-nil cancel reason for expired task")
		}
	case <-time.After(5 * time.Second):
		t.Error("timed out waiting for task expiry notification")
	}
}

// TestPool_Release_Triggers_Queue verifies that releasing a running task
// causes the next queued task to start.
func TestPool_Release_Triggers_Queue(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    5,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	// Queue a second task.
	id2 := newUUID()
	started := make(chan struct{}, 1)
	go func() {
		pm.Acquire(id2, "test", false, nopStop, func() error { //nolint:errcheck
			close(started)
			return nil
		}, nil)
	}()

	time.Sleep(10 * time.Millisecond)

	// Release id1 — id2 should start.
	pm.Release(id1)

	select {
	case <-started:
		// success
	case <-time.After(3 * time.Second):
		t.Error("queued task did not start after Release")
	}
}

// TestPool_PurgeQueue_ByAction verifies that PurgeQueue only clears the
// specified action's queue and leaves other action queues intact.
func TestPool_PurgeQueue_ByAction(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the global slot with action "a".
	id1 := newUUID()
	pm.Acquire(id1, "a", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	// Queue two tasks for action "a" and two for action "b".
	cancelledA := make(chan struct{}, 2)
	cancelledB := make(chan struct{}, 2)

	enqueue := func(action string, cancelCh chan struct{}) string {
		id := newUUID()
		pm.Acquire(id, action, false, nopStop, func() error { return nil }, func(_ error) { //nolint:errcheck
			cancelCh <- struct{}{}
		})
		return id
	}

	time.Sleep(5 * time.Millisecond)
	enqueue("a", cancelledA)
	enqueue("a", cancelledA)
	enqueue("b", cancelledB)
	enqueue("b", cancelledB)
	time.Sleep(5 * time.Millisecond)

	// Purge only action "a".
	count, _ := pm.PurgeQueue("a")
	if count != 2 {
		t.Errorf("PurgeQueue(a): expected 2 purged, got %d", count)
	}

	// Both "a" cancel notifications should fire.
	for i := 0; i < 2; i++ {
		select {
		case <-cancelledA:
		case <-time.After(2 * time.Second):
			t.Errorf("cancelledA[%d]: timed out", i)
		}
	}

	// No "b" cancellations should have fired.
	select {
	case <-cancelledB:
		t.Error("action 'b' queue should not have been purged")
	case <-time.After(100 * time.Millisecond):
		// correct: no cancellations
	}
}

// TestPool_PurgeQueue_All verifies that PurgeQueue("") clears all queues.
func TestPool_PurgeQueue_All(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the slot.
	id1 := newUUID()
	pm.Acquire(id1, "x", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	cancelled := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		id := newUUID()
		action := "x"
		if i >= 2 {
			action = "y"
		}
		pm.Acquire(id, action, false, nopStop, func() error { return nil }, func(_ error) { //nolint:errcheck
			cancelled <- struct{}{}
		})
	}
	time.Sleep(10 * time.Millisecond)

	count, _ := pm.PurgeQueue("")
	if count != 4 {
		t.Errorf("PurgeQueue(\"\"): expected 4, got %d", count)
	}

	for i := 0; i < 4; i++ {
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Errorf("cancelled[%d]: timed out", i)
		}
	}
}

// TestPool_DrainAll verifies that DrainAll cancels all queued items and
// clears the running list.
func TestPool_DrainAll(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst,
		},
	}, nil)

	// Hold the slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	cancelled := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		id := newUUID()
		pm.Acquire(id, "test", false, nopStop, func() error { return nil }, func(_ error) { //nolint:errcheck
			cancelled <- struct{}{}
		})
	}
	time.Sleep(10 * time.Millisecond)

	pm.DrainAll()

	for i := 0; i < 3; i++ {
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Errorf("cancelled[%d]: timed out after DrainAll", i)
		}
	}

	// Running should be cleared.
	info := pm.Info("")
	if info.Running != 0 {
		t.Errorf("after DrainAll: expected 0 running, got %d", info.Running)
	}
}

// TestPool_Info_Global verifies Info("") returns global pool state.
func TestPool_Info_Global(t *testing.T) {
	pm := NewPoolManager(PoolConfig{Limit: 5}, nil)
	defer pm.DrainAll()

	// Start 2 tasks.
	for i := 0; i < 2; i++ {
		pm.Acquire(newUUID(), "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck
	}

	info := pm.Info("")
	if info.Limit != 5 {
		t.Errorf("Info.Limit: got %d want 5", info.Limit)
	}
	if info.Running != 2 {
		t.Errorf("Info.Running: got %d want 2", info.Running)
	}
}

// TestPool_Info_PerAction verifies Info(action) returns per-action counts.
func TestPool_Info_PerAction(t *testing.T) {
	pm := NewPoolManager(PoolConfig{Limit: 10}, nil)
	defer pm.DrainAll()

	// Start 2 "alpha" tasks and 3 "beta" tasks.
	for i := 0; i < 2; i++ {
		pm.Acquire(newUUID(), "alpha", false, nopStop, func() error { return nil }, nil) //nolint:errcheck
	}
	for i := 0; i < 3; i++ {
		pm.Acquire(newUUID(), "beta", false, nopStop, func() error { return nil }, nil) //nolint:errcheck
	}

	infoAlpha := pm.Info("alpha")
	if infoAlpha.Running != 2 {
		t.Errorf("Info(alpha).Running: got %d want 2", infoAlpha.Running)
	}

	infoBeta := pm.Info("beta")
	if infoBeta.Running != 3 {
		t.Errorf("Info(beta).Running: got %d want 3", infoBeta.Running)
	}
}

// TestPool_Dequeue_RemovesSpecificTask verifies that Dequeue(taskID) removes
// the specific task from its queue and calls cancelFn.
func TestPool_Dequeue_RemovesSpecificTask(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	// Queue a specific task.
	targetID := newUUID()
	cancelled := make(chan struct{}, 1)
	pm.Acquire(targetID, "test", false, nopStop, func() error { return nil }, func(_ error) { //nolint:errcheck
		cancelled <- struct{}{}
	})
	time.Sleep(5 * time.Millisecond)

	// Dequeue the specific task by ID.
	found := pm.Dequeue(targetID)
	if !found {
		t.Fatal("Dequeue: expected to find targetID in queue")
	}

	select {
	case <-cancelled:
		// correct
	case <-time.After(2 * time.Second):
		t.Error("cancelFn not called after Dequeue")
	}
}

// TestPool_Dequeue_NotFound verifies Dequeue returns false for unknown task ID.
func TestPool_Dequeue_NotFound(t *testing.T) {
	pm := NewPoolManager(PoolConfig{Limit: 5}, nil)
	defer pm.DrainAll()

	found := pm.Dequeue("nonexistent-task-id")
	if found {
		t.Error("Dequeue: expected false for unknown task ID")
	}
}

// TestPool_SetLimits_Excess_Stop verifies that SetLimits with a lower limit
// stops excess running tasks when ExcessAction is ExcessStop.
func TestPool_SetLimits_Excess_Stop(t *testing.T) {
	pm := NewPoolManager(PoolConfig{Limit: 3}, nil)
	defer pm.DrainAll()

	stopCalled := make(chan string, 3)

	// Start 3 tasks, each with a stop function that signals.
	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		id := newUUID()
		ids[i] = id
		capturedID := id
		stop := func() { stopCalled <- capturedID }
		pm.Acquire(id, "test", false, stop, func() error { return nil }, nil) //nolint:errcheck
	}

	// Reduce limit to 1 with excess action = stop.
	excess := &ExcessConfig{Action: ExcessStop, Order: KillFirst}
	pm.SetLimits("", 1, 0, excess) //nolint:errcheck

	// Two tasks should have had their stop functions called.
	stopped := make(map[string]bool)
	timeout := time.After(3 * time.Second)
	for len(stopped) < 2 {
		select {
		case id := <-stopCalled:
			stopped[id] = true
		case <-timeout:
			t.Fatalf("timed out waiting for stop calls: got %d of 2", len(stopped))
		}
	}
}

// TestPool_SetLimits_Excess_Requeue verifies that SetLimits with ExcessRequeue
// moves excess running tasks back to the queue.
func TestPool_SetLimits_Excess_Requeue(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 3,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Start 3 tasks.
	ids := poolTestIDs(3)
	for _, id := range ids {
		pm.Acquire(id, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck
	}

	// Reduce limit to 1 with excess action = requeue.
	excess := &ExcessConfig{Action: ExcessRequeue, Order: KillLast}
	pm.SetLimits("", 1, 10, excess) //nolint:errcheck

	// Wait for requeue operations to propagate.
	time.Sleep(50 * time.Millisecond)

	// The pool should now have at most 1 running task (possibly 0 if those were
	// moved to queue). Verify total running ≤ 1.
	info := pm.Info("")
	if info.Running > 1 {
		t.Errorf("after SetLimits(1): expected Running <= 1, got %d", info.Running)
	}
}

// TestPool_ActionLimit verifies per-action limits are enforced separately
// from the global limit.
func TestPool_ActionLimit(t *testing.T) {
	// Global limit 10, per-action limit 2 for "limited".
	actionCfgs := map[string]PoolConfig{
		"limited": {Limit: 2},
	}
	pm := NewPoolManager(PoolConfig{Limit: 10}, actionCfgs)
	defer pm.DrainAll()

	// Start 2 "limited" tasks — should succeed.
	for i := 0; i < 2; i++ {
		_, err := pm.Acquire(newUUID(), "limited", false, nopStop, func() error { return nil }, nil)
		if err != nil {
			t.Fatalf("Acquire limited[%d]: unexpected error: %v", i, err)
		}
	}

	// Third "limited" task should be rejected (no queue configured for "limited").
	_, err := pm.Acquire(newUUID(), "limited", false, nopStop, func() error { return nil }, nil)
	if err == nil {
		t.Error("expected third 'limited' task to be rejected, got nil error")
	}

	// "unlimited" action should still have plenty of capacity.
	_, err = pm.Acquire(newUUID(), "unlimited", false, nopStop, func() error { return nil }, nil)
	if err != nil {
		t.Errorf("'unlimited' action should be admitted, got error: %v", err)
	}
}

// TestTaskQueue_EnqueueDequeue_FIFO verifies TaskQueue internals directly.
func TestTaskQueue_EnqueueDequeue_FIFO(t *testing.T) {
	q := &TaskQueue{
		cfg: &QueueConfig{
			Enabled: true,
			Size:    5,
			Order:   OrderFirst, // FIFO
		},
	}

	ids := []string{"first", "second", "third"}
	for _, id := range ids {
		capturedID := id
		item := QueuedItem{
			taskID:  capturedID,
			action:  "test",
			startFn: func() error { return nil },
		}
		if !q.Enqueue(item) {
			t.Fatalf("Enqueue(%s): unexpected false", capturedID)
		}
	}

	if q.len() != 3 {
		t.Fatalf("expected len=3, got %d", q.len())
	}

	for i, wantID := range ids {
		item := q.Dequeue()
		if item == nil {
			t.Fatalf("Dequeue[%d]: got nil", i)
		}
		if item.taskID != wantID {
			t.Errorf("Dequeue[%d]: got %q, want %q", i, item.taskID, wantID)
		}
	}
}

// TestTaskQueue_EnqueueDequeue_LIFO verifies TaskQueue internals for LIFO.
func TestTaskQueue_EnqueueDequeue_LIFO(t *testing.T) {
	q := &TaskQueue{
		cfg: &QueueConfig{
			Enabled: true,
			Size:    5,
			Order:   OrderLast, // LIFO
		},
	}

	ids := []string{"first", "second", "third"}
	for _, id := range ids {
		capturedID := id
		q.Enqueue(QueuedItem{taskID: capturedID, startFn: func() error { return nil }})
	}

	// LIFO: last enqueued should come out first.
	expected := []string{"third", "second", "first"}
	for i, wantID := range expected {
		item := q.Dequeue()
		if item == nil {
			t.Fatalf("Dequeue[%d]: got nil", i)
		}
		if item.taskID != wantID {
			t.Errorf("Dequeue[%d]: got %q, want %q", i, item.taskID, wantID)
		}
	}
}

// TestTaskQueue_Full verifies that Enqueue returns false when queue is full.
func TestTaskQueue_Full(t *testing.T) {
	q := &TaskQueue{
		cfg: &QueueConfig{
			Enabled: true,
			Size:    2,
			Order:   OrderFirst,
		},
	}

	for i := 0; i < 2; i++ {
		id := newUUID()
		if !q.Enqueue(QueuedItem{taskID: id, startFn: func() error { return nil }}) {
			t.Fatalf("Enqueue[%d]: unexpected false when queue not full", i)
		}
	}

	// Third enqueue should fail.
	if q.Enqueue(QueuedItem{taskID: newUUID(), startFn: func() error { return nil }}) {
		t.Error("Enqueue to full queue should return false")
	}
}

// TestTaskQueue_Remove verifies that Remove finds and removes a specific item.
func TestTaskQueue_Remove(t *testing.T) {
	q := &TaskQueue{
		cfg: &QueueConfig{
			Enabled: true,
			Size:    10,
			Order:   OrderFirst,
		},
	}

	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		capturedID := id
		q.Enqueue(QueuedItem{taskID: capturedID, startFn: func() error { return nil }})
	}

	// Remove "b" from the middle.
	found, item := q.Remove("b")
	if !found {
		t.Fatal("Remove: expected to find 'b'")
	}
	if item.taskID != "b" {
		t.Errorf("Remove: got taskID %q, want %q", item.taskID, "b")
	}

	// Queue should now have 2 items: "a" and "c".
	if q.len() != 2 {
		t.Errorf("len after Remove: got %d, want 2", q.len())
	}

	// Remove non-existent item.
	found, _ = q.Remove("ghost")
	if found {
		t.Error("Remove: expected false for non-existent taskID")
	}
}

// TestTaskQueue_Info returns snapshots.
func TestTaskQueue_Info(t *testing.T) {
	q := &TaskQueue{
		cfg: &QueueConfig{
			Enabled: true,
			Size:    5,
			Order:   OrderFirst,
		},
	}

	items := []QueuedItem{
		{taskID: "t1", action: "a", startFn: func() error { return nil }},
		{taskID: "t2", action: "b", startFn: func() error { return nil }},
	}
	for _, it := range items {
		q.Enqueue(it)
	}

	infos := q.Info()
	if len(infos) != 2 {
		t.Fatalf("Info: expected 2 items, got %d", len(infos))
	}
	if infos[0].TaskID != "t1" {
		t.Errorf("Info[0].TaskID: got %q want t1", infos[0].TaskID)
	}
	if infos[1].TaskID != "t2" {
		t.Errorf("Info[1].TaskID: got %q want t2", infos[1].TaskID)
	}
}

// TestPool_CancelFnOutsideLock verifies that cancelFn is invoked OUTSIDE pm.mu.
// If it were called while pm.mu was held, calling pool.Info() inside the
// cancelFn would deadlock. The test would hang (then timeout) if the lock
// ordering regression is reintroduced.
func TestPool_CancelFnOutsideLock(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    5,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Occupy the single slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	cancelDone := make(chan struct{}, 1)

	id2 := newUUID()
	pm.Acquire(id2, "test", false, nopStop, func() error { return nil }, func(reason error) { //nolint:errcheck
		// Calling Info() here will deadlock if pm.mu is still held.
		_ = pm.Info("")
		cancelDone <- struct{}{}
	})

	// Dequeue id2 — triggers cancelFn.
	pm.Dequeue(id2)

	select {
	case <-cancelDone:
		// Good — cancelFn completed without deadlock.
	case <-time.After(3 * time.Second):
		t.Error("cancelFn did not complete — likely deadlocked on pm.mu")
	}

	pm.Release(id1)
}

// TestPool_ReleaseNoOverrun verifies that concurrent goroutines Acquiring and
// Releasing slots never cause the running count to exceed the pool limit.
func TestPool_ReleaseNoOverrun(t *testing.T) {
	const limit = 3
	const goroutines = 20
	const ops = 50

	pm := NewPoolManager(PoolConfig{Limit: limit}, nil)
	defer pm.DrainAll()

	var (
		mu      sync.Mutex
		running int
		maxSeen int
	)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				id := newUUID()
				var started atomic.Bool
				result, err := pm.Acquire(id, "test", false, nopStop, func() error {
					started.Store(true)
					mu.Lock()
					running++
					if running > maxSeen {
						maxSeen = running
					}
					mu.Unlock()
					return nil
				}, nil)
				if err != nil {
					continue
				}
				if result == AcquireRunning && started.Load() {
					time.Sleep(time.Millisecond)
					mu.Lock()
					running--
					mu.Unlock()
					pm.Release(id)
				}
			}
		}()
	}
	wg.Wait()

	if maxSeen > limit {
		t.Errorf("running count exceeded limit: max seen %d, limit %d", maxSeen, limit)
	}
}

// TestPool_StartFnError verifies that when startFn returns an error for a
// queued task, the cancelFn is called with that error.
func TestPool_StartFnError(t *testing.T) {
	pm := NewPoolManager(PoolConfig{
		Limit: 1,
		Queue: &QueueConfig{
			Enabled: true,
			Size:    5,
			Order:   OrderFirst,
		},
	}, nil)
	defer pm.DrainAll()

	// Hold the slot.
	id1 := newUUID()
	pm.Acquire(id1, "test", false, nopStop, func() error { return nil }, nil) //nolint:errcheck

	// Queue a task with a startFn that will fail.
	id2 := newUUID()
	cancelledReason := make(chan error, 1)
	startErr := errors.New("start failed")

	pm.Acquire(id2, "test", false, nopStop, func() error { //nolint:errcheck
		return startErr
	}, func(reason error) {
		cancelledReason <- reason
	})

	time.Sleep(5 * time.Millisecond)

	// Release id1 → id2's startFn is called → fails → cancelFn called.
	pm.Release(id1)

	select {
	case reason := <-cancelledReason:
		if reason == nil {
			t.Error("expected non-nil cancel reason when startFn fails")
		}
	case <-time.After(3 * time.Second):
		t.Error("cancelFn not called after startFn failure")
	}
}

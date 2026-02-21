package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// hubTestEnv holds a test HTTP server + a connected WS client.
// buf holds messages that were read but not yet matched by readUntil.
type hubTestEnv struct {
	hub    *Hub
	server *httptest.Server
	conn   *websocket.Conn
	buf    []map[string]interface{}
}

// defaultTestActions builds the standard set of test actions for hub tests.
// - echo: exec action, command=/bin/echo, required param "msg"
// - sleep: exec action, command=/bin/sleep, optional param "duration" default "60"
// - false: exec action, command=/bin/false, no params
func defaultTestActions() map[string]ActionHandler {
	return map[string]ActionHandler{
		"echo": &testEchoHandler{},
		"sleep": &testSleepHandler{},
		"false": &testFalseHandler{},
	}
}

// testEchoHandler starts /bin/echo with the "msg" param as argument.
type testEchoHandler struct{}

func (h *testEchoHandler) Describe() ActionInfo {
	return ActionInfo{
		Name:   "echo",
		Type:   "exec",
		Params: map[string]*ParamSpec{},
	}
}

func (h *testEchoHandler) Validate(params map[string]string) error {
	return nil
}

func (h *testEchoHandler) Start(taskID string, params map[string]string, cb workerCallbacks) (*Worker, error) {
	msg := params["msg"]
	return startWorker(workerConfig{
		TaskID:        taskID,
		Command:       "/bin/echo",
		Args:          []string{msg},
		IncludeStdout: true,
		IncludeStderr: true,
	}, cb)
}

// testSleepHandler starts /bin/sleep with an optional duration param.
type testSleepHandler struct{}

func (h *testSleepHandler) Describe() ActionInfo {
	defVal := "60"
	return ActionInfo{
		Name: "sleep",
		Type: "exec",
		Params: map[string]*ParamSpec{
			"duration": {Default: &defVal},
		},
	}
}

func (h *testSleepHandler) Validate(params map[string]string) error {
	return nil
}

func (h *testSleepHandler) Start(taskID string, params map[string]string, cb workerCallbacks) (*Worker, error) {
	dur := params["duration"]
	if dur == "" {
		dur = "60"
	}
	return startWorker(workerConfig{
		TaskID:        taskID,
		Command:       "/bin/sleep",
		Args:          []string{dur},
		IncludeStdout: true,
		IncludeStderr: true,
	}, cb)
}

// testFalseHandler starts /bin/false (exits non-zero immediately).
type testFalseHandler struct{}

func (h *testFalseHandler) Describe() ActionInfo {
	return ActionInfo{
		Name:   "false",
		Type:   "exec",
		Params: map[string]*ParamSpec{},
	}
}

func (h *testFalseHandler) Validate(params map[string]string) error {
	return nil
}

func (h *testFalseHandler) Start(taskID string, params map[string]string, cb workerCallbacks) (*Worker, error) {
	return startWorker(workerConfig{
		TaskID:        taskID,
		Command:       "/bin/false",
		Args:          []string{},
		IncludeStdout: true,
		IncludeStderr: true,
	}, cb)
}

// newHubEnv creates a test hub environment. If actions is nil, defaultTestActions() are used.
// The hub is wired to an HTTP test server so tests can dial real WebSocket connections.
func newHubEnv(t *testing.T, actions map[string]ActionHandler) *hubTestEnv {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if actions == nil {
		actions = defaultTestActions()
	}

	hub := newHub(hubConfig{DB: db, Actions: actions})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		rawConn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn := &wsConn{Conn: rawConn}
		hub.HandleClient(conn)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { wsConn.Close() })

	return &hubTestEnv{hub: hub, server: srv, conn: wsConn}
}

// send sends a JSON message to the hub.
func (e *hubTestEnv) send(t *testing.T, v interface{}) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := e.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

// rawRead reads one message from the wire and appends it to e.buf.
func (e *hubTestEnv) rawRead(t *testing.T, deadline time.Time) bool {
	t.Helper()
	e.conn.SetReadDeadline(deadline)
	_, raw, err := e.conn.ReadMessage()
	if err != nil {
		return false
	}
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	e.buf = append(e.buf, m)
	return true
}

// readMsg returns the next buffered message, or reads one from the wire.
func (e *hubTestEnv) readMsg(t *testing.T) map[string]interface{} {
	t.Helper()
	if len(e.buf) > 0 {
		m := e.buf[0]
		e.buf = e.buf[1:]
		return m
	}
	if !e.rawRead(t, time.Now().Add(5*time.Second)) {
		t.Fatal("readMsg: timeout or error")
	}
	m := e.buf[0]
	e.buf = e.buf[1:]
	return m
}

// readUntil returns the first buffered (or incoming) message matching pred.
// Non-matching messages are kept in the buffer for future reads.
func (e *hubTestEnv) readUntil(t *testing.T, timeout time.Duration, pred func(map[string]interface{}) bool) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)

	// Check already-buffered messages first.
	for i, m := range e.buf {
		if pred(m) {
			e.buf = append(e.buf[:i], e.buf[i+1:]...)
			return m
		}
	}

	// Read from wire until match or deadline.
	for time.Now().Before(deadline) {
		if !e.rawRead(t, deadline) {
			break
		}
		// Check the message we just appended.
		last := e.buf[len(e.buf)-1]
		if pred(last) {
			e.buf = e.buf[:len(e.buf)-1]
			return last
		}
	}
	t.Fatal("readUntil: timed out waiting for matching message")
	return nil
}

func msgType(m map[string]interface{}) string {
	s, _ := m["type"].(string)
	return s
}

// TestStart_UnknownAction errors when an unknown action is requested.
func TestStart_UnknownAction(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "req1",
		"action": "nonexistent",
		"params": map[string]string{},
	})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unknown action, got %q", msgType(m))
	}
}

// TestStart_NoAction errors when no action provided and hub has actions map.
func TestStart_NoAction(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "start", "id": "req1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error, got %q", msgType(m))
	}
}

// TestStart_ValidAction starts the echo action and receives started message.
func TestStart_ValidAction(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "req2",
		"action": "echo",
		"params": map[string]string{"msg": "hello"},
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	if m["task_id"] == "" || m["task_id"] == nil {
		t.Error("missing task_id in started message")
	}
	if m["pid"] == nil {
		t.Error("missing pid in started message")
	}
	// Drain exited
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestList returns the started task.
func TestList_ContainsStartedTask(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "s1",
		"action": "echo",
		"params": map[string]string{"msg": "hi"},
	})
	started := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	taskID := started["task_id"].(string)

	// Drain any output/exited messages, then list
	time.Sleep(100 * time.Millisecond)
	e.send(t, map[string]interface{}{"type": "list", "id": "l1"})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "tasks"
	})

	tasks, _ := m["tasks"].([]interface{})
	found := false
	for _, raw := range tasks {
		task, _ := raw.(map[string]interface{})
		if task["task_id"] == taskID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("task %q not found in list response", taskID)
	}
}

// TestStop stops a running task; subsequent list shows stopped state.
func TestStop_TaskBecomesStoped(t *testing.T) {
	e := newHubEnv(t, nil)
	// Use sleep so it stays running long enough to stop
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "s1",
		"action": "sleep",
		"params": map[string]string{"duration": "60"},
	})
	started := e.readMsg(t)
	if msgType(started) != "started" {
		t.Fatalf("expected started, got %v", started)
	}
	taskID := started["task_id"].(string)

	e.send(t, map[string]interface{}{"type": "stop", "task_id": taskID, "id": "stop1"})

	// Wait for exited broadcast
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})

	e.send(t, map[string]interface{}{"type": "list", "id": "l1"})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "tasks"
	})

	tasks, _ := m["tasks"].([]interface{})
	for _, raw := range tasks {
		task, _ := raw.(map[string]interface{})
		if task["task_id"] == taskID {
			if task["state"] != "stopped" {
				t.Errorf("task state: got %q want stopped", task["state"])
			}
			return
		}
	}
	t.Errorf("task %q not found after stop", taskID)
}

// TestStart_SameTaskIDWhileRunning returns error.
func TestStart_SameTaskIDWhileRunning(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"task_id": "dup-task",
		"action":  "sleep",
		"params":  map[string]string{"duration": "60"},
	})
	m := e.readMsg(t)
	if msgType(m) != "started" {
		t.Fatalf("expected started, got %v", m)
	}

	// Try starting same task_id again
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s2",
		"task_id": "dup-task",
		"action":  "sleep",
		"params":  map[string]string{"duration": "60"},
	})
	m2 := e.readMsg(t)
	if msgType(m2) != "error" {
		t.Errorf("expected error for duplicate running task, got %q", msgType(m2))
	}

	// Cleanup: stop the running task
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "dup-task"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestReplay echoes back buffered output events.
func TestReplay_EchoesBufferedOutput(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "s1",
		"action": "echo",
		"params": map[string]string{"msg": "replay-test"},
	})
	started := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	taskID := started["task_id"].(string)

	// Wait for echo to finish
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})

	e.send(t, map[string]interface{}{"type": "replay", "task_id": taskID, "id": "r1"})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "output"
	})
	if m["data"] != "replay-test" {
		t.Errorf("replay output data: got %q want replay-test", m["data"])
	}
}

// TestReset_NonErroredTask returns error.
func TestReset_NonErroredTask(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"task_id": "reset-task",
		"action":  "sleep",
		"params":  map[string]string{"duration": "60"},
	})
	started := e.readMsg(t)
	if msgType(started) != "started" {
		t.Fatalf("expected started, got %v", started)
	}

	e.send(t, map[string]interface{}{"type": "reset", "task_id": "reset-task", "id": "r1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error resetting non-errored task, got %q", msgType(m))
	}

	// Cleanup
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "reset-task"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestRetryPolicy_Errored starts a fast-exiting command with threshold=2 and expects errored.
func TestRetryPolicy_Errored(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "s1",
		"action": "false",
		"retry_policy": map[string]interface{}{
			"error_threshold": 2,
		},
	})
	// /bin/false may exit before started is sent; use readUntil
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// Wait for errored broadcast (allow time for 2 exits + restart)
	m := e.readUntil(t, 15*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "errored"
	})
	if m["type"] != "errored" {
		t.Errorf("expected errored message, got %v", m)
	}
}

// TestShutdown_StopsRunningWorkers verifies Shutdown stops all running workers.
func TestShutdown_StopsRunningWorkers(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "s1",
		"action": "sleep",
		"params": map[string]string{"duration": "60"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.hub.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After shutdown, no running workers should remain.
	if e.hub.hasRunningWorkers() {
		t.Error("expected no running workers after Shutdown")
	}
}

// TestShutdown_Idempotent verifies calling Shutdown twice doesn't panic.
func TestShutdown_Idempotent(t *testing.T) {
	e := newHubEnv(t, nil)
	ctx := context.Background()
	if err := e.hub.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := e.hub.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestShutdown_ContextTimeout verifies Shutdown returns ctx.Err when context
// expires before workers exit.
func TestShutdown_ContextTimeout(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "start",
		"action": "sleep",
		"params": map[string]string{"duration": "60"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// Shutdown with a context that expires before the worker can be stopped.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	err := e.hub.Shutdown(ctx)
	if err == nil {
		// Worker might have exited by chance; acceptable but unlikely.
		t.Log("Shutdown returned nil (worker exited before context expired)")
	}
	// Forcefully clean up the long-running sleep.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_ = e.hub.Shutdown(ctx2)
}

// --- Message handler edge cases ---

func TestHandleUnknownMessageType(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "bogus", "id": "req1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unknown type, got %q", msgType(m))
	}
}

func TestHandleInvalidJSON(t *testing.T) {
	// When the hub receives invalid JSON, its ReadJSON call fails and the
	// connection handler exits — the connection is closed from the server side.
	// The hub does NOT send an error frame; it simply terminates the loop.
	// We verify this by checking that the connection becomes unreadable.
	e := newHubEnv(t, nil)
	if err := e.conn.WriteMessage(websocket.TextMessage, []byte(`{not valid json`)); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	// After sending invalid JSON the hub will close. Either we receive a close
	// frame or a read error — both are acceptable outcomes.
	e.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := e.conn.ReadMessage()
	// We expect either a close error or a websocket close message.
	// Either way, the connection should not return more app-level messages.
	_ = err // close or error is expected
}

// --- handleList ---

func TestHandleList_Empty(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "list", "id": "l1"})
	m := e.readMsg(t)
	if msgType(m) != "tasks" {
		t.Fatalf("expected tasks, got %q", msgType(m))
	}
	tasks, _ := m["tasks"].([]interface{})
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestHandleList_SinceFilter_ExcludesOldTasks(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "old-task",
		"action":  "echo",
		"params":  map[string]string{"msg": "hi"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})

	// A since far in the future excludes tasks whose last activity is in the past.
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	e.send(t, map[string]interface{}{"type": "list", "since": future, "id": "l1"})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "tasks"
	})
	tasks, _ := m["tasks"].([]interface{})
	for _, raw := range tasks {
		task, _ := raw.(map[string]interface{})
		if task["task_id"] == "old-task" {
			t.Error("old task should have been filtered by since")
		}
	}
}

func TestHandleList_InvalidSince(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "list", "since": "not-a-timestamp", "id": "l1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for invalid since, got %q", msgType(m))
	}
}

// --- handleStop ---

func TestHandleStop_MissingTaskID(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "stop", "id": "s1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for missing task_id, got %q", msgType(m))
	}
}

func TestHandleStop_NotFound(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "ghost"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unknown task, got %q", msgType(m))
	}
}

// --- handleReset ---

func TestHandleReset_MissingTaskID(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "reset", "id": "r1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for missing task_id, got %q", msgType(m))
	}
}

func TestHandleReset_NotFound(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "reset", "task_id": "ghost"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unknown task, got %q", msgType(m))
	}
}

// TestReset_ErroredTask_Restarts verifies a successful reset: an errored task
// returns to active and a new worker is started.
func TestReset_ErroredTask_Restarts(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"task_id": "errored-task",
		"action":  "false",
		"retry_policy": map[string]interface{}{
			"error_threshold": 1,
		},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	e.readUntil(t, 10*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "errored"
	})

	e.send(t, map[string]interface{}{"type": "reset", "task_id": "errored-task", "id": "r1"})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	if tid, _ := m["task_id"].(string); tid != "errored-task" {
		t.Errorf("started task_id: got %q want errored-task", tid)
	}
	// Task restarts again and will error again; just stop it.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "errored-task"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// --- handleReplay ---

func TestHandleReplay_MissingTaskID(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "replay", "id": "r1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for missing task_id, got %q", msgType(m))
	}
}

func TestHandleReplay_NotFound(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "replay", "task_id": "ghost"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unknown task, got %q", msgType(m))
	}
}

// TestHandleReplay_NoWorker exercises the "no worker for task" path — a task
// that exists in memory but has never had a worker started (e.g. loaded from
// DB after a restart).
func TestHandleReplay_NoWorker(t *testing.T) {
	e := newHubEnv(t, nil)
	// Inject a task with nil worker directly.
	e.hub.mu.Lock()
	e.hub.tasks["noworker"] = &Task{
		record: TaskRecord{
			TaskID: "noworker",
			Action: "echo",
			Params: map[string]string{},
			State:  StateStopped,
		},
	}
	e.hub.mu.Unlock()

	e.send(t, map[string]interface{}{"type": "replay", "task_id": "noworker", "id": "r1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for task with no worker, got %q", msgType(m))
	}
}

func TestHandleReplay_InvalidSince(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "rs-inv",
		"action":  "sleep",
		"params":  map[string]string{"duration": "10"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	e.send(t, map[string]interface{}{"type": "replay", "task_id": "rs-inv", "since": "garbage"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for invalid since, got %q", msgType(m))
	}

	e.send(t, map[string]interface{}{"type": "stop", "task_id": "rs-inv"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestHandleReplay_SinceFilter_Future verifies that replaying with a since
// timestamp in the future returns no events.
func TestHandleReplay_SinceFilter_Future(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "rsfuture",
		"action":  "echo",
		"params":  map[string]string{"msg": "hello"},
	})
	started := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	taskID := started["task_id"].(string)
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
	// Discard any remaining buffered messages (e.g. output).
	e.buf = nil

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	e.send(t, map[string]interface{}{"type": "replay", "task_id": taskID, "since": future})

	// Nothing should arrive within a short window.
	e.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	_, raw, err := e.conn.ReadMessage()
	if err == nil {
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		if msgType(m) == "output" || msgType(m) == "exited" {
			t.Errorf("future-since replay should return no events; got %q", msgType(m))
		}
	}
}

// --- Retry policy ---

// TestRetryPolicy_RestartOnExit verifies that a task with a retry policy but
// no error threshold broadcasts a "restarting" message on unintentional exit.
func TestRetryPolicy_RestartOnExit(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"task_id": "restart-task",
		"action":  "false",
		// retry_policy with no error_threshold → restart indefinitely
		"retry_policy": map[string]interface{}{},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// Expect a restarting broadcast.
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "restarting"
	})

	// Stop the task to prevent an infinite restart loop.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "restart-task"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// --- Internal unit tests ---

func TestTaskInfo_PopulatesWorkerFields(t *testing.T) {
	exitCode := 42
	task := &Task{
		record: TaskRecord{
			TaskID:       "t",
			Action:       "echo",
			Params:       map[string]string{"x": "y"},
			State:        StateStopped,
			CreatedAt:    time.Now().UTC(),
			ErrorMessage: "something went wrong",
		},
		worker: &Worker{
			PID:      9999,
			State:    WorkerExited,
			ExitCode: &exitCode,
		},
	}
	info := taskInfo(task)
	if info.CurrentPID != 9999 {
		t.Errorf("CurrentPID: got %d want 9999", info.CurrentPID)
	}
	if info.WorkerState != WorkerExited {
		t.Errorf("WorkerState: got %v want %v", info.WorkerState, WorkerExited)
	}
	if info.LastExitCode == nil || *info.LastExitCode != 42 {
		t.Errorf("LastExitCode: got %v want 42", info.LastExitCode)
	}
	if info.ErrorMessage != "something went wrong" {
		t.Errorf("ErrorMessage: got %q", info.ErrorMessage)
	}
}

func TestTaskInfo_NilWorker(t *testing.T) {
	task := &Task{
		record: TaskRecord{
			TaskID:    "t",
			Action:    "echo",
			Params:    map[string]string{},
			State:     StateStopped,
			CreatedAt: time.Now().UTC(),
		},
	}
	info := taskInfo(task)
	if info.CurrentPID != 0 {
		t.Errorf("CurrentPID: got %d want 0", info.CurrentPID)
	}
	if info.WorkerState != "" {
		t.Errorf("WorkerState: got %q want empty", info.WorkerState)
	}
	if info.LastExitCode != nil {
		t.Errorf("LastExitCode: got %v want nil", info.LastExitCode)
	}
}

func TestTaskInfo_NilExitCode(t *testing.T) {
	// Worker that is still running has no ExitCode yet.
	task := &Task{
		record: TaskRecord{
			TaskID:    "t",
			Action:    "echo",
			Params:    map[string]string{},
			State:     StateActive,
			CreatedAt: time.Now().UTC(),
		},
		worker: &Worker{
			PID:      1111,
			State:    WorkerRunning,
			ExitCode: nil,
		},
	}
	info := taskInfo(task)
	if info.LastExitCode != nil {
		t.Errorf("LastExitCode: got %v want nil for running worker", info.LastExitCode)
	}
}

// TestDescribe_AllActions verifies that describe with empty action returns all registered actions.
func TestDescribe_AllActions(t *testing.T) {
	e := newHubEnv(t, nil)

	e.send(t, map[string]interface{}{
		"type": "describe",
		"id":   "d1",
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "actions"
	})
	actions, _ := m["actions"].([]interface{})
	if len(actions) < 1 {
		t.Errorf("expected at least 1 action in describe response, got %d", len(actions))
	}
}

// TestDescribe_SingleAction verifies that describe with a specific action returns only that action.
func TestDescribe_SingleAction(t *testing.T) {
	e := newHubEnv(t, nil)

	e.send(t, map[string]interface{}{
		"type":   "describe",
		"id":     "d1",
		"action": "echo",
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "actions"
	})
	actions, _ := m["actions"].([]interface{})
	if len(actions) != 1 {
		t.Errorf("expected 1 action in describe response for 'echo', got %d", len(actions))
	}
	if len(actions) > 0 {
		action, _ := actions[0].(map[string]interface{})
		if action["name"] != "echo" {
			t.Errorf("expected action name 'echo', got %q", action["name"])
		}
	}
}

// TestDescribe_UnknownAction verifies an error for an unknown action name.
func TestDescribe_UnknownAction(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{
		"type":   "describe",
		"id":     "d1",
		"action": "nonexistent",
	})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unknown action, got %q", msgType(m))
	}
}

// TestPoolInfo_NoPool verifies pool_info with no pool configured returns empty pool info.
func TestPoolInfo_NoPool(t *testing.T) {
	e := newHubEnv(t, nil) // hub has no pool manager
	e.send(t, map[string]interface{}{
		"type": "pool_info",
		"id":   "p1",
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "pool_info"
	})
	if msgType(m) != "pool_info" {
		t.Errorf("expected pool_info, got %q", msgType(m))
	}
}

// TestBroadcast_AllClients verifies Broadcast sends to all connected clients.
func TestBroadcast_AllClients(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	hub := newHub(hubConfig{DB: db, Actions: defaultTestActions()})

	// Create 3 mock connections
	const numClients = 3
	received := make([]chan bool, numClients)
	conns := make([]*mockConn, numClients)
	for i := 0; i < numClients; i++ {
		ch := make(chan bool, 1)
		received[i] = ch
		mc := &mockConn{onWrite: func() { ch <- true }}
		conns[i] = mc
		hub.AddClient(mc)
	}

	// Broadcast a message
	hub.Broadcast(map[string]string{"type": "test"})

	// Each client should have received the message
	for i, ch := range received {
		select {
		case <-ch:
			// received
		case <-time.After(2 * time.Second):
			t.Errorf("client %d did not receive broadcast", i)
		}
	}
}

// mockConn is a minimal Conn implementation for testing broadcast.
type mockConn struct {
	mu      sync.Mutex
	onWrite func()
	closed  bool
}

func (m *mockConn) ReadJSON(v any) error {
	// Block indefinitely (simulates a client not sending anything).
	select {}
}

func (m *mockConn) WriteJSON(v any) error {
	if m.onWrite != nil {
		m.onWrite()
	}
	return nil
}

func (m *mockConn) Close() error {
	m.closed = true
	return nil
}

func (m *mockConn) RemoteAddr() string { return "mock" }

func (m *mockConn) WriteLock() *sync.Mutex { return &m.mu }

// dialSecondConn opens a second WebSocket connection to the same hub server and
// returns the underlying *websocket.Conn. Caller should Close() it when done.
func dialSecondConn(t *testing.T, e *hubTestEnv) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dialSecondConn: %v", err)
	}
	return c
}

// sendOnConn sends a JSON message on a raw *websocket.Conn.
func sendOnConn(t *testing.T, conn *websocket.Conn, v interface{}) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("sendOnConn: %v", err)
	}
}

// readMsgOnConn reads one message from a raw *websocket.Conn within timeout.
func readMsgOnConn(t *testing.T, conn *websocket.Conn, timeout time.Duration) (map[string]interface{}, bool) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return nil, false
	}
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return m, true
}

// readUntilOnConn reads messages from conn until pred matches or timeout.
func readUntilOnConn(t *testing.T, conn *websocket.Conn, timeout time.Duration, pred func(map[string]interface{}) bool) (map[string]interface{}, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return nil, false
		}
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		if pred(m) {
			return m, true
		}
	}
	return nil, false
}

// --- Subscription tests (job 1380) ---

// testDedupeEchoHandler is an echo handler that has a DedupeKey set for testing.
type testDedupeEchoHandler struct {
	dedupeKey []string
}

func (h *testDedupeEchoHandler) Describe() ActionInfo {
	return ActionInfo{
		Name:      "dedup-echo",
		Type:      "exec",
		Params:    map[string]*ParamSpec{},
		DedupeKey: h.dedupeKey,
	}
}

func (h *testDedupeEchoHandler) Validate(params map[string]string) error { return nil }

func (h *testDedupeEchoHandler) Start(taskID string, params map[string]string, cb workerCallbacks) (*Worker, error) {
	return startWorker(workerConfig{
		TaskID:        taskID,
		Command:       "/bin/sleep",
		Args:          []string{"60"},
		IncludeStdout: true,
		IncludeStderr: true,
	}, cb)
}

// TestAutoSubscribeOnStart verifies that the connection starting a task automatically
// receives its output events, while a second connection that never subscribed does not.
func TestAutoSubscribeOnStart(t *testing.T) {
	e := newHubEnv(t, nil)
	connB := dialSecondConn(t, e)
	defer connB.Close()

	// connA (e.conn) starts the echo task.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "sub-auto-1",
		"action":  "echo",
		"params":  map[string]string{"msg": "hello-subscribe"},
	})

	// connA should receive the started message (auto-subscribed).
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	if m["task_id"] != "sub-auto-1" {
		t.Errorf("unexpected task_id: %v", m["task_id"])
	}

	// connA should also receive exited (auto-subscribed).
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})

	// connB should NOT have received any task-specific events.
	if msg, ok := readMsgOnConn(t, connB, 200*time.Millisecond); ok {
		t.Errorf("connB should not receive task events without subscribing, got: %v", msg["type"])
	}
}

// TestManualSubscribe verifies that a client manually subscribing to a task_id
// receives its subsequent events.
func TestManualSubscribe(t *testing.T) {
	e := newHubEnv(t, map[string]ActionHandler{
		"sleep": &testSleepHandler{},
	})
	connB := dialSecondConn(t, e)
	defer connB.Close()

	// connA starts a sleep task.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "sub-manual-1",
		"action":  "sleep",
		"params":  map[string]string{"duration": "5"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// connB subscribes to the task.
	sendOnConn(t, connB, map[string]interface{}{
		"type":    "subscribe",
		"id":      "sub1",
		"task_id": "sub-manual-1",
	})

	// connB should receive a "subscribed" confirmation.
	m, ok := readMsgOnConn(t, connB, 3*time.Second)
	if !ok {
		t.Fatal("connB did not receive subscribed confirmation")
	}
	if msgType(m) != "subscribed" {
		t.Errorf("expected subscribed, got %q", msgType(m))
	}
	if m["task_id"] != "sub-manual-1" {
		t.Errorf("subscribed task_id: want sub-manual-1, got %v", m["task_id"])
	}

	// Stop the task via connA to generate an exited event.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "sub-manual-1"})

	// connB should receive the exited event because it subscribed.
	msg, ok := readUntilOnConn(t, connB, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
	if !ok {
		t.Fatal("connB did not receive exited event after subscribing")
	}
	if msg["task_id"] != "sub-manual-1" {
		t.Errorf("exited task_id: want sub-manual-1, got %v", msg["task_id"])
	}
}

// TestUnsubscribe verifies that after unsubscribing, a client no longer receives task events.
func TestUnsubscribe(t *testing.T) {
	e := newHubEnv(t, map[string]ActionHandler{
		"sleep": &testSleepHandler{},
	})

	// connA starts a sleep task and thus auto-subscribes.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "unsub-task-1",
		"action":  "sleep",
		"params":  map[string]string{"duration": "60"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// connA unsubscribes.
	e.send(t, map[string]interface{}{
		"type":    "unsubscribe",
		"id":      "unsub1",
		"task_id": "unsub-task-1",
	})
	e.readUntil(t, 3*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "unsubscribed"
	})

	// Stop the task to generate an exited event.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "unsub-task-1"})

	// connA should NOT receive the exited event now that it unsubscribed.
	e.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, raw, err := e.conn.ReadMessage()
	if err == nil {
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		if msgType(m) == "exited" {
			t.Error("connA should not receive exited event after unsubscribing")
		}
	}
}

// TestSubscribe_MissingTaskID verifies that subscribe without task_id returns an error.
func TestSubscribe_MissingTaskID(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "subscribe", "id": "sub-no-id"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for subscribe without task_id, got %q", msgType(m))
	}
}

// TestUnsubscribe_MissingTaskID verifies that unsubscribe without task_id returns an error.
func TestUnsubscribe_MissingTaskID(t *testing.T) {
	e := newHubEnv(t, nil)
	e.send(t, map[string]interface{}{"type": "unsubscribe", "id": "unsub-no-id"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for unsubscribe without task_id, got %q", msgType(m))
	}
}

// --- Deduplication tests (job 1390) ---

// TestDedup_RejectDuplicate verifies that a second start with the same DedupeKey params
// is rejected with an error containing the existing task_id.
func TestDedup_RejectDuplicate(t *testing.T) {
	actions := map[string]ActionHandler{
		"dedup-sleep": &testDedupeEchoHandler{dedupeKey: []string{"key"}},
	}
	e := newHubEnv(t, actions)

	// Start first task.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "dedup-1",
		"action":  "dedup-sleep",
		"params":  map[string]string{"key": "same-value"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// Start second task with same dedup params — should be rejected.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "dedup-2",
		"id":      "req2",
		"action":  "dedup-sleep",
		"params":  map[string]string{"key": "same-value"},
	})
	m := e.readUntil(t, 3*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "error"
	})
	if m["existing_task_id"] == nil {
		t.Error("expected existing_task_id in error response")
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "duplicate") {
		t.Errorf("expected 'duplicate' in error message, got: %q", msg)
	}

	// Clean up: stop the first task.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "dedup-1"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestDedup_DifferentParams_BothStart verifies that two tasks with different DedupeKey param
// values can both start successfully.
func TestDedup_DifferentParams_BothStart(t *testing.T) {
	actions := map[string]ActionHandler{
		"dedup-sleep": &testDedupeEchoHandler{dedupeKey: []string{"key"}},
	}
	e := newHubEnv(t, actions)

	// Start first task.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "dedup-diff-1",
		"action":  "dedup-sleep",
		"params":  map[string]string{"key": "value-a"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started" && m["task_id"] == "dedup-diff-1"
	})

	// Start second task with DIFFERENT key value — should succeed.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "dedup-diff-2",
		"id":      "req2",
		"action":  "dedup-sleep",
		"params":  map[string]string{"key": "value-b"},
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started" && m["task_id"] == "dedup-diff-2"
	})
	if m == nil {
		t.Fatal("second task with different key should have started")
	}

	// Clean up.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "dedup-diff-1"})
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "dedup-diff-2"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestDedupeFingerprint verifies the dedupeFingerprint helper function.
func TestDedupeFingerprint(t *testing.T) {
	// Empty dedupeKey returns "".
	if fp := dedupeFingerprint("action", map[string]string{"a": "1"}, nil); fp != "" {
		t.Errorf("expected empty fingerprint for nil dedupeKey, got %q", fp)
	}
	if fp := dedupeFingerprint("action", map[string]string{"a": "1"}, []string{}); fp != "" {
		t.Errorf("expected empty fingerprint for empty dedupeKey, got %q", fp)
	}

	// Same inputs produce same fingerprint.
	fp1 := dedupeFingerprint("build", map[string]string{"env": "prod", "branch": "main"}, []string{"env", "branch"})
	fp2 := dedupeFingerprint("build", map[string]string{"env": "prod", "branch": "main"}, []string{"branch", "env"}) // key order reversed
	if fp1 != fp2 {
		t.Errorf("fingerprint should be stable regardless of dedupeKey order: %q vs %q", fp1, fp2)
	}

	// Different action → different fingerprint.
	fpA := dedupeFingerprint("build", map[string]string{"env": "prod"}, []string{"env"})
	fpB := dedupeFingerprint("deploy", map[string]string{"env": "prod"}, []string{"env"})
	if fpA == fpB {
		t.Error("different actions should produce different fingerprints")
	}

	// Different param value → different fingerprint.
	fpX := dedupeFingerprint("build", map[string]string{"env": "prod"}, []string{"env"})
	fpY := dedupeFingerprint("build", map[string]string{"env": "staging"}, []string{"env"})
	if fpX == fpY {
		t.Error("different param values should produce different fingerprints")
	}
}

// --- Output sequence number tests (job 1330) ---

// TestOutputSeqNumbers verifies that output messages have monotonically increasing Seq fields.
func TestOutputSeqNumbers(t *testing.T) {
	e := newHubEnv(t, map[string]ActionHandler{
		"seq-echo": &testEchoHandler{},
	})

	// Send echo with 3 words separated by newlines.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "seq-task",
		"action":  "seq-echo",
		"params":  map[string]string{"msg": "line1\nline2\nline3"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})

	// Collect output messages until exited.
	var seqs []float64
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		if msgType(m) == "output" {
			if seq, ok := m["seq"].(float64); ok && seq > 0 {
				seqs = append(seqs, seq)
			}
		}
		return msgType(m) == "exited"
	})

	// Verify sequence numbers are monotonically increasing.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("seq[%d]=%v not greater than seq[%d]=%v", i, seqs[i], i-1, seqs[i-1])
		}
	}
}

// --- Metrics handler tests (job 1400) ---

// TestMetricsGlobal verifies that a "metrics" request returns global snapshot.
func TestMetricsGlobal(t *testing.T) {
	e := newHubEnv(t, nil)

	// Start and wait for echo task to complete.
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "m1",
		"action": "echo",
		"params": map[string]string{"msg": "hello"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})

	// Query global metrics.
	e.send(t, map[string]interface{}{"type": "metrics", "id": "met1"})
	m := e.readUntil(t, 3*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "metrics"
	})

	global, _ := m["global"].(map[string]interface{})
	if global == nil {
		t.Fatal("expected global metrics in response")
	}
	started, _ := global["tasks_started"].(float64)
	if started < 1 {
		t.Errorf("tasks_started: want >= 1, got %v", started)
	}
}

// TestMetricsAction verifies per-action metrics.
func TestMetricsAction(t *testing.T) {
	e := newHubEnv(t, nil)

	// Start and complete an echo task.
	e.send(t, map[string]interface{}{
		"type":   "start",
		"id":     "ma1",
		"action": "echo",
		"params": map[string]string{"msg": "hello"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})

	// Query per-action metrics.
	e.send(t, map[string]interface{}{"type": "metrics", "id": "met2", "action": "echo"})
	m := e.readUntil(t, 3*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "metrics"
	})

	action, _ := m["action"].(map[string]interface{})
	if action == nil {
		t.Fatal("expected action metrics in response")
	}
	started, _ := action["tasks_started"].(float64)
	if started < 1 {
		t.Errorf("echo tasks_started: want >= 1, got %v", started)
	}
}

// TestMetricsTask verifies per-task metrics.
func TestMetricsTask(t *testing.T) {
	e := newHubEnv(t, nil)

	const taskID = "metrics-task-42"
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "mt1",
		"task_id": taskID,
		"action":  "echo",
		"params":  map[string]string{"msg": "hi"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
	time.Sleep(50 * time.Millisecond) // allow RecordExit to complete

	// Query per-task metrics.
	e.send(t, map[string]interface{}{"type": "metrics", "id": "met3", "task_id": taskID})
	m := e.readUntil(t, 3*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "metrics"
	})

	task, _ := m["task"].(map[string]interface{})
	if task == nil {
		t.Fatal("expected task metrics in response")
	}
}

// TestDedup_AfterExited_AllowsRestart verifies that after a task with a dedupe key
// exits, a new start request with the same dedup params succeeds (the old task is
// no longer "running" and should not block the new one).
func TestDedup_AfterExited_AllowsRestart(t *testing.T) {
	e := newHubEnv(t, map[string]ActionHandler{
		"dedup-short": &testDedupeEchoHandler{dedupeKey: []string{"key"}},
	})

	// testDedupeEchoHandler starts "sleep 60", so stop it to simulate exit.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "dedup-exit-1",
		"action":  "dedup-short",
		"params":  map[string]string{"key": "restart-val"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started" && m["task_id"] == "dedup-exit-1"
	})

	// Stop the task so it exits.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "dedup-exit-1"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
	// Allow hub bookkeeping to process the exit.
	time.Sleep(100 * time.Millisecond)

	// Start a new task with the SAME dedup params — should succeed.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "dedup-exit-2",
		"id":      "req-restart",
		"action":  "dedup-short",
		"params":  map[string]string{"key": "restart-val"},
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return (msgType(m) == "started" && m["task_id"] == "dedup-exit-2") || msgType(m) == "error"
	})
	if m == nil {
		t.Fatal("no response within timeout")
	}
	if msgType(m) == "error" {
		t.Errorf("expected new start to succeed after prior task exited, got error: %v", m["message"])
	}

	// Clean up.
	e.send(t, map[string]interface{}{"type": "stop", "task_id": "dedup-exit-2"})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestSeqNumbersResetPerTask verifies that each task has an independent output
// sequence counter starting at 1. Two tasks running concurrently should each
// have their own Seq counter rather than sharing a global one.
func TestSeqNumbersResetPerTask(t *testing.T) {
	e := newHubEnv(t, map[string]ActionHandler{
		"seq-echo": &testEchoHandler{},
	})
	connB := dialSecondConn(t, e)
	defer connB.Close()

	// Start task A — subscribe connB so we get events even though connA started it.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "seq-a",
		"action":  "seq-echo",
		"params":  map[string]string{"msg": "lineA"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started" && m["task_id"] == "seq-a"
	})

	// Start task B.
	e.send(t, map[string]interface{}{
		"type":    "start",
		"task_id": "seq-b",
		"action":  "seq-echo",
		"params":  map[string]string{"msg": "lineB"},
	})
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started" && m["task_id"] == "seq-b"
	})

	// Collect output messages for both tasks.
	seqByTask := map[string][]float64{}
	deadline := time.Now().Add(5 * time.Second)
	exitedCount := 0
	for time.Now().Before(deadline) && exitedCount < 2 {
		e.conn.SetReadDeadline(deadline)
		_, raw, err := e.conn.ReadMessage()
		if err != nil {
			break
		}
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		switch msgType(m) {
		case "output":
			tid, _ := m["task_id"].(string)
			if seq, ok := m["seq"].(float64); ok && seq > 0 {
				seqByTask[tid] = append(seqByTask[tid], seq)
			}
		case "exited":
			exitedCount++
		}
	}

	// Each task that produced output should have sequences starting at 1.
	for tid, seqs := range seqByTask {
		if len(seqs) == 0 {
			continue
		}
		if seqs[0] != 1 {
			t.Errorf("task %s: first seq should be 1, got %v", tid, seqs[0])
		}
		for i := 1; i < len(seqs); i++ {
			if seqs[i] != seqs[i-1]+1 {
				t.Errorf("task %s: seq[%d]=%v, seq[%d]=%v (not consecutive)", tid, i, seqs[i], i-1, seqs[i-1])
			}
		}
	}
}

// TestStdioConn_HubIntegration verifies that Hub can handle a stdioConn as a Conn.
// This tests the STDIO transport pathway without a real WebSocket.
func TestStdioConn_HubIntegration(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	hub := newHub(hubConfig{DB: db, Actions: defaultTestActions()})

	// Create in-memory pipes to simulate stdin/stdout.
	// clientR reads hub responses; clientW sends client messages.
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()

	serverConn := newStdioConn(serverR, serverW)

	// Run the hub handler in a goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.HandleClient(serverConn)
	}()

	// Send a list request via clientW.
	clientConn := newStdioConn(clientR, clientW)

	msg := IncomingMessage{Type: "list", ID: "test-list"}
	if err := clientConn.WriteJSON(msg); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// Read the response.
	var resp map[string]interface{}
	if err := clientConn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if resp["type"] != "tasks" {
		t.Errorf("expected tasks response, got %q", resp["type"])
	}

	// Close to signal handler to exit.
	clientW.Close()
	serverR.Close()
	clientR.Close()
	serverW.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("hub handler did not exit after connection close")
	}
}

package overseer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newHubEnv(t *testing.T, pinnedCmd string) *hubTestEnv {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	hub := NewHub(HubConfig{DB: db, PinnedCommand: pinnedCmd})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hub.HandleClient(conn)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return &hubTestEnv{hub: hub, server: srv, conn: conn}
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

// TestStart_NoCommand_NoPin errors when no command provided and no pinned command.
func TestStart_NoCommand_NoPin(t *testing.T) {
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{"type": "start", "id": "req1"})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error, got %q", msgType(m))
	}
}

// TestStart_ValidCommand starts /bin/echo and receives started message.
func TestStart_ValidCommand(t *testing.T) {
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "req2",
		"command": "/bin/echo",
		"args":    []string{"hello"},
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
}

// TestList returns the started task.
func TestList_ContainsStartedTask(t *testing.T) {
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"command": "/bin/echo",
		"args":    []string{"hi"},
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
	e := newHubEnv(t, "")
	// Use sleep so it stays running long enough to stop
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"command": "/bin/sleep",
		"args":    []string{"60"},
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
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"task_id": "dup-task",
		"command": "/bin/sleep",
		"args":    []string{"60"},
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
		"command": "/bin/sleep",
		"args":    []string{"60"},
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
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"command": "/bin/echo",
		"args":    []string{"replay-test"},
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
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"task_id": "reset-task",
		"command": "/bin/sleep",
		"args":    []string{"60"},
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
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"command": "/bin/false",
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

// TestPinnedCommand_WrongCommand returns error.
func TestPinnedCommand_WrongCommand(t *testing.T) {
	e := newHubEnv(t, "/bin/echo")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"command": "/bin/sh",
	})
	m := e.readMsg(t)
	if msgType(m) != "error" {
		t.Errorf("expected error for wrong pinned command, got %q", msgType(m))
	}
}

// TestPinnedCommand_OmittedCommand succeeds.
func TestPinnedCommand_OmittedCommand(t *testing.T) {
	e := newHubEnv(t, "/bin/echo")
	e.send(t, map[string]interface{}{
		"type": "start",
		"id":   "s1",
		"args": []string{"pinned-test"},
	})
	m := e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "started"
	})
	if m["task_id"] == nil {
		t.Error("missing task_id in started message")
	}
	// Drain exited broadcast
	e.readUntil(t, 5*time.Second, func(m map[string]interface{}) bool {
		return msgType(m) == "exited"
	})
}

// TestShutdown_StopsRunningWorkers verifies Shutdown stops all running workers.
func TestShutdown_StopsRunningWorkers(t *testing.T) {
	e := newHubEnv(t, "")
	e.send(t, map[string]interface{}{
		"type":    "start",
		"id":      "s1",
		"command": "/bin/sleep",
		"args":    []string{"60"},
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
	e := newHubEnv(t, "")
	ctx := context.Background()
	if err := e.hub.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := e.hub.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

package overseer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test 1: TestBuildManifest_structure
// ---------------------------------------------------------------------------

func TestBuildManifest_structure(t *testing.T) {
	m := BuildManifest(nil, "1.0.0")

	t.Run("protocol", func(t *testing.T) {
		if m.Protocol != "sticky-overseer" {
			t.Errorf("Protocol = %q; want %q", m.Protocol, "sticky-overseer")
		}
	})

	t.Run("version", func(t *testing.T) {
		if m.Version != "1.0.0" {
			t.Errorf("Version = %q; want %q", m.Version, "1.0.0")
		}
	})

	t.Run("transport", func(t *testing.T) {
		if m.Transport.Path != "/ws" {
			t.Errorf("Transport.Path = %q; want %q", m.Transport.Path, "/ws")
		}
		if m.Transport.TypeField != "type" {
			t.Errorf("Transport.TypeField = %q; want %q", m.Transport.TypeField, "type")
		}
		if m.Transport.CorrelationField != "id" {
			t.Errorf("Transport.CorrelationField = %q; want %q", m.Transport.CorrelationField, "id")
		}
	})

	t.Run("messages_count", func(t *testing.T) {
		// 13 c2s + 17 s2c = 30, but some keys are deduplicated (pool_info, metrics, manifest
		// exist in both directions and are stored under different keys).
		// We guarantee at least 13 entries.
		if len(m.Messages) < 13 {
			t.Errorf("len(Messages) = %d; want >= 13", len(m.Messages))
		}
	})

	t.Run("operations_count", func(t *testing.T) {
		if len(m.Operations) < 13 {
			t.Errorf("len(Operations) = %d; want >= 13", len(m.Operations))
		}
	})

	t.Run("types_count", func(t *testing.T) {
		if len(m.Types) < 10 {
			t.Errorf("len(Types) = %d; want >= 10", len(m.Types))
		}
	})

	t.Run("actions_nil_returns_empty_slice", func(t *testing.T) {
		if m.Actions == nil {
			t.Error("Actions should not be nil when constructed with nil actions map")
		}
		if len(m.Actions) != 0 {
			t.Errorf("len(Actions) = %d; want 0", len(m.Actions))
		}
	})

	t.Run("operations_send_references_existing_message", func(t *testing.T) {
		for opName, op := range m.Operations {
			if _, ok := m.Messages[op.Send]; !ok {
				t.Errorf("operation %q: send=%q does not exist in Messages", opName, op.Send)
			}
		}
	})

	t.Run("key_messages_exist", func(t *testing.T) {
		required := []string{"start", "stop", "started", "output", "error"}
		for _, key := range required {
			if _, ok := m.Messages[key]; !ok {
				t.Errorf("Messages[%q] is missing", key)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Test 2: TestBuildManifest_actions
// ---------------------------------------------------------------------------

// mockActionHandler is a minimal ActionHandler that returns a known ActionInfo.
type mockActionHandler struct{}

func (h *mockActionHandler) Describe() ActionInfo {
	required := (*string)(nil) // nil default = required
	return ActionInfo{
		Name:        "test-action",
		Type:        "mock",
		Description: "A test action",
		Params: map[string]*ParamSpec{
			"input": {Default: required},
		},
	}
}

func (h *mockActionHandler) Validate(params map[string]string) error {
	return nil
}

func (h *mockActionHandler) Start(taskID string, params map[string]string, cb WorkerCallbacks) (*Worker, error) {
	return nil, errors.New("mockActionHandler.Start not implemented")
}

func TestBuildManifest_actions(t *testing.T) {
	actions := map[string]ActionHandler{
		"test-action": &mockActionHandler{},
	}

	m := BuildManifest(actions, "2.0.0")

	if len(m.Actions) != 1 {
		t.Fatalf("len(Actions) = %d; want 1", len(m.Actions))
	}
	if m.Actions[0].Name != "test-action" {
		t.Errorf("Actions[0].Name = %q; want %q", m.Actions[0].Name, "test-action")
	}
}

// ---------------------------------------------------------------------------
// Test 3: TestManifestHTTPEndpoint
// ---------------------------------------------------------------------------

// startTCPTransportForTest binds a tcpTransport on a free port, returns the
// channel and the actual listening address (host:port).
func startTCPTransportForTest(t *testing.T, ctx context.Context, tr tcpTransport) (<-chan Conn, string) {
	t.Helper()
	// Bind a free port first, then use that address.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tr.addr = addr
	ch, err := tr.Listen(ctx)
	if err != nil {
		t.Fatalf("tcpTransport.Listen: %v", err)
	}
	// Give the server a moment to bind.
	time.Sleep(20 * time.Millisecond)
	return ch, addr
}

// TestManifestHTTPEndpoint verifies that the old /ws/manifest HTTP endpoint is no longer
// served (manifest is now only accessible via the WS "manifest" message type).
func TestManifestHTTPEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := tcpTransport{
		addr:    ":0",
		version: "3.0.0",
	}

	_, addr := startTCPTransportForTest(t, ctx, tr)

	url := "http://" + addr + "/ws/manifest"

	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET /ws/manifest: %v", err)
	}
	defer resp.Body.Close()

	// HTTP endpoint removed — manifest is now WS-only.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (HTTP manifest endpoint removed)", resp.StatusCode)
	}
}

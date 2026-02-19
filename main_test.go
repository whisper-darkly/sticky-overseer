package overseer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsTrusted_Loopback(t *testing.T) {
	nets := DetectLocalSubnets()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:9000"
	if !isTrusted(req, nets) {
		t.Error("127.0.0.1 should be trusted")
	}
}

func TestIsTrusted_OutOfRange(t *testing.T) {
	nets := DetectLocalSubnets()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:9000" // TEST-NET-3 (RFC 5737)
	if isTrusted(req, nets) {
		t.Error("203.0.113.1 should not be trusted by local subnets")
	}
}

func TestIsTrusted_EmptyNets_AllowAll(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:9000"
	if !isTrusted(req, nil) {
		t.Error("empty nets slice should allow all IPs")
	}
}

func TestNewUUID_Format(t *testing.T) {
	uuid := newUUID()
	parts := splitDash(uuid)
	if len(parts) != 5 {
		t.Fatalf("expected 5 dash-separated parts, got %d: %q", len(parts), uuid)
	}
	lens := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != lens[i] {
			t.Errorf("part %d: expected len %d, got %d (%q)", i, lens[i], len(p), p)
		}
	}
}

func TestNewUUID_NoCollisions(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		u := newUUID()
		if _, exists := seen[u]; exists {
			t.Fatalf("collision at iteration %d: %q", i, u)
		}
		seen[u] = struct{}{}
	}
}

func TestRetryPolicy_JSONRoundTrip(t *testing.T) {
	rp := RetryPolicy{
		RestartDelay:   30 * time.Second,
		ErrorWindow:    5 * time.Minute,
		ErrorThreshold: 3,
	}
	b, err := json.Marshal(rp)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var got RetryPolicy
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if got.RestartDelay != 30*time.Second {
		t.Errorf("RestartDelay: got %v want 30s", got.RestartDelay)
	}
	if got.ErrorWindow != 5*time.Minute {
		t.Errorf("ErrorWindow: got %v want 5m", got.ErrorWindow)
	}
	if got.ErrorThreshold != 3 {
		t.Errorf("ErrorThreshold: got %d want 3", got.ErrorThreshold)
	}
}

func TestRetryPolicy_UnmarshalString(t *testing.T) {
	data := []byte(`{"restart_delay":"30s","error_window":"5m","error_threshold":3}`)
	var rp RetryPolicy
	if err := json.Unmarshal(data, &rp); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if rp.RestartDelay != 30*time.Second {
		t.Errorf("RestartDelay: got %v want 30s", rp.RestartDelay)
	}
	if rp.ErrorWindow != 5*time.Minute {
		t.Errorf("ErrorWindow: got %v want 5m", rp.ErrorWindow)
	}
}

func TestRetryPolicy_InvalidDuration(t *testing.T) {
	data := []byte(`{"restart_delay":"invalid"}`)
	var rp RetryPolicy
	if err := json.Unmarshal(data, &rp); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestRetryPolicy_MarshalOmitsZeroDurations(t *testing.T) {
	rp := RetryPolicy{ErrorThreshold: 5}
	b, err := json.Marshal(rp)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	if _, ok := m["restart_delay"]; ok {
		t.Error("restart_delay should be omitted when zero")
	}
	if _, ok := m["error_window"]; ok {
		t.Error("error_window should be omitted when zero")
	}
}

func TestParseTrustedCIDRs_Empty(t *testing.T) {
	nets, err := ParseTrustedCIDRs("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nets != nil {
		t.Error("expected nil nets for empty string")
	}
}

func TestParseTrustedCIDRs_Valid(t *testing.T) {
	nets, err := ParseTrustedCIDRs("127.0.0.1,10.0.0.0/8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("expected 2 nets, got %d", len(nets))
	}
}

func TestParseTrustedCIDRs_Invalid(t *testing.T) {
	_, err := ParseTrustedCIDRs("not-an-ip")
	if err == nil {
		t.Error("expected error for invalid IP")
	}
}

// splitDash splits a string on '-' without importing strings.
func splitDash(s string) []string {
	var parts []string
	start := 0
	for i, c := range s {
		if c == '-' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

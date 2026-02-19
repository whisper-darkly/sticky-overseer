package overseer

import (
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestParseDuration_Valid(t *testing.T) {
	cases := []struct {
		s    string
		want string
	}{
		{"30s", "30s"},
		{"5m", "5m0s"},
		{"1h30m", "1h30m0s"},
	}
	for _, c := range cases {
		d, err := parseDuration(c.s)
		if err != nil {
			t.Errorf("parseDuration(%q): unexpected error: %v", c.s, err)
			continue
		}
		if d.String() != c.want {
			t.Errorf("parseDuration(%q): got %q want %q", c.s, d.String(), c.want)
		}
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	cases := []string{"", "abc", "1d", "5 seconds"}
	for _, s := range cases {
		_, err := parseDuration(s)
		if err == nil {
			t.Errorf("parseDuration(%q): expected error but got none", s)
		}
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

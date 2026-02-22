package overseer

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_structure — verifies Swagger 2.0 envelope
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_structure(t *testing.T) {
	data := BuildOpenAPISpec(nil, "v1.2.3")

	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("BuildOpenAPISpec returned invalid JSON: %v", err)
	}

	if got, ok := spec["swagger"].(string); !ok || got != "2.0" {
		t.Errorf(`swagger = %v; want "2.0"`, spec["swagger"])
	}

	info, ok := spec["info"].(map[string]any)
	if !ok {
		t.Fatal("info field missing or wrong type")
	}
	if info["title"] != "sticky-overseer" {
		t.Errorf(`info.title = %v; want "sticky-overseer"`, info["title"])
	}
	if info["version"] != "v1.2.3" {
		t.Errorf(`info.version = %v; want "v1.2.3"`, info["version"])
	}

	// With nil actions, we still get the standard WS command paths.
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("paths field missing or wrong type")
	}
	// Standard protocol commands: list, stop, reset, replay, describe, pool_info,
	// purge, set_pool, subscribe, unsubscribe, metrics, manifest = 12 paths.
	if len(paths) != 12 {
		t.Errorf("expected 12 standard WS command paths for nil actions, got %d", len(paths))
	}

	// Verify x-websocket-endpoints is present.
	xws, ok := spec["x-websocket-endpoints"].(map[string]any)
	if !ok {
		t.Fatal("x-websocket-endpoints field missing or wrong type")
	}
	ws, ok := xws["/ws"].(map[string]any)
	if !ok {
		t.Fatal("x-websocket-endpoints['/ws'] missing or wrong type")
	}
	if ws["type_field"] != "type" {
		t.Errorf(`x-websocket-endpoints["/ws"].type_field = %v; want "type"`, ws["type_field"])
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_actions — verifies action paths and params
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_actions(t *testing.T) {
	defVal := "world"
	actions := map[string]ActionHandler{
		"greet": &stubHandler{
			info: ActionInfo{
				Name:        "greet",
				Type:        "exec",
				Description: "Greet someone",
				Params: map[string]*ParamSpec{
					"name":   nil, // required
					"suffix": {Default: &defVal, Validate: `value != ""`},
				},
			},
		},
		"echo": &stubHandler{
			info: ActionInfo{
				Name: "echo",
				Type: "exec",
			},
		},
	}

	data := BuildOpenAPISpec(actions, "2.0.0")
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	paths, _ := spec["paths"].(map[string]any)
	// 12 standard WS commands + 2 action paths = 14.
	if len(paths) != 14 {
		t.Fatalf("expected 14 paths (12 standard + 2 actions), got %d", len(paths))
	}

	// Verify greet path exists at /ws/actions/greet with ws method.
	greetPath, ok := paths["/ws/actions/greet"].(map[string]any)
	if !ok {
		t.Fatal("missing /ws/actions/greet path")
	}
	wsOp, ok := greetPath["ws"].(map[string]any)
	if !ok {
		t.Fatal("missing ws operation for /ws/actions/greet")
	}

	// Check operationId.
	if wsOp["operationId"] != "start-greet" {
		t.Errorf("operationId = %v; want start-greet", wsOp["operationId"])
	}

	// Check x-action-type.
	if wsOp["x-action-type"] != "exec" {
		t.Errorf("x-action-type = %v; want exec", wsOp["x-action-type"])
	}

	// Check x-ws-message and x-ws-action.
	if wsOp["x-ws-message"] != "start" {
		t.Errorf("x-ws-message = %v; want start", wsOp["x-ws-message"])
	}
	if wsOp["x-ws-action"] != "greet" {
		t.Errorf("x-ws-action = %v; want greet", wsOp["x-ws-action"])
	}
	if wsOp["x-ws-params-key"] != "params" {
		t.Errorf("x-ws-params-key = %v; want params", wsOp["x-ws-params-key"])
	}

	// Check parameters include both name (required) and suffix (optional).
	rawParams, _ := wsOp["parameters"].([]any)
	if len(rawParams) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(rawParams))
	}

	// Required param should come first.
	p0, _ := rawParams[0].(map[string]any)
	if p0["name"] != "name" {
		t.Errorf("first param name = %v; want name", p0["name"])
	}
	if p0["required"] != true {
		t.Errorf("name param required = %v; want true", p0["required"])
	}

	p1, _ := rawParams[1].(map[string]any)
	if p1["name"] != "suffix" {
		t.Errorf("second param name = %v; want suffix", p1["name"])
	}
	if p1["required"] != false {
		t.Errorf("suffix param required = %v; want false", p1["required"])
	}
	if p1["default"] != defVal {
		t.Errorf("suffix param default = %v; want %q", p1["default"], defVal)
	}
	if !strings.Contains(p1["x-validate"].(string), "value") {
		t.Errorf("suffix x-validate = %v; expected CEL expression", p1["x-validate"])
	}

	// Verify echo path exists with no parameters.
	echoPath, ok := paths["/ws/actions/echo"].(map[string]any)
	if !ok {
		t.Fatal("missing /ws/actions/echo path")
	}
	echoWS, _ := echoPath["ws"].(map[string]any)
	echoParams := echoWS["parameters"]
	if echoParams != nil {
		t.Errorf("echo should have no parameters, got %v", echoParams)
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_tags — verifies tags list
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_tags(t *testing.T) {
	actions := map[string]ActionHandler{
		"beta":  &stubHandler{info: ActionInfo{Name: "beta", Type: "exec"}},
		"alpha": &stubHandler{info: ActionInfo{Name: "alpha", Type: "exec", Description: "Alpha action"}},
	}

	data := BuildOpenAPISpec(actions, "1.0.0")
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	tags, _ := spec["tags"].([]any)
	// ws-commands + alpha + beta = 3 tags.
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags (ws-commands + 2 actions), got %d", len(tags))
	}

	// ws-commands tag should be first.
	first, _ := tags[0].(map[string]any)
	if first["name"] != "ws-commands" {
		t.Errorf("first tag = %v; want ws-commands", first["name"])
	}

	// Action tags should be sorted alphabetically after ws-commands.
	second, _ := tags[1].(map[string]any)
	if second["name"] != "alpha" {
		t.Errorf("second tag = %v; want alpha (alphabetical order)", second["name"])
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_extensions — verifies x-task-pool, x-retry, x-dedupe-key
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_extensions(t *testing.T) {
	actions := map[string]ActionHandler{
		"worker": &stubHandler{
			info: ActionInfo{
				Name:      "worker",
				Type:      "exec",
				TaskPool:  PoolConfig{Limit: 3},
				DedupeKey: []string{"url"},
			},
		},
	}

	data := BuildOpenAPISpec(actions, "1.0.0")
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	paths, _ := spec["paths"].(map[string]any)
	path, _ := paths["/ws/actions/worker"].(map[string]any)
	wsOp, _ := path["ws"].(map[string]any)

	pool, ok := wsOp["x-task-pool"].(map[string]any)
	if !ok {
		t.Fatal("x-task-pool extension missing")
	}
	if pool["limit"].(float64) != 3 {
		t.Errorf("x-task-pool.limit = %v; want 3", pool["limit"])
	}

	dedupeKey, _ := wsOp["x-dedupe-key"].([]any)
	if len(dedupeKey) != 1 || dedupeKey[0] != "url" {
		t.Errorf("x-dedupe-key = %v; want [url]", dedupeKey)
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_wsCommands — verifies standard WS command paths
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_wsCommands(t *testing.T) {
	data := BuildOpenAPISpec(nil, "1.0.0")
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	paths, _ := spec["paths"].(map[string]any)

	expectedCommands := []struct {
		path    string
		message string
	}{
		{"/ws/list", "list"},
		{"/ws/stop", "stop"},
		{"/ws/reset", "reset"},
		{"/ws/replay", "replay"},
		{"/ws/describe", "describe"},
		{"/ws/pool_info", "pool_info"},
		{"/ws/purge", "purge"},
		{"/ws/set_pool", "set_pool"},
		{"/ws/subscribe", "subscribe"},
		{"/ws/unsubscribe", "unsubscribe"},
		{"/ws/metrics", "metrics"},
		{"/ws/manifest", "manifest"},
	}

	for _, tc := range expectedCommands {
		pathItem, ok := paths[tc.path].(map[string]any)
		if !ok {
			t.Errorf("missing path %s", tc.path)
			continue
		}
		wsOp, ok := pathItem["ws"].(map[string]any)
		if !ok {
			t.Errorf("missing ws operation at %s", tc.path)
			continue
		}
		if wsOp["x-ws-message"] != tc.message {
			t.Errorf("%s x-ws-message = %v; want %s", tc.path, wsOp["x-ws-message"], tc.message)
		}
		if wsOp["x-ws-endpoint"] != "/ws" {
			t.Errorf("%s x-ws-endpoint = %v; want /ws", tc.path, wsOp["x-ws-endpoint"])
		}
	}

	// Verify /ws/stop has required task_id param.
	stopItem, _ := paths["/ws/stop"].(map[string]any)
	stopOp, _ := stopItem["ws"].(map[string]any)
	stopParams, _ := stopOp["parameters"].([]any)
	if len(stopParams) != 1 {
		t.Fatalf("/ws/stop expected 1 param, got %d", len(stopParams))
	}
	p0, _ := stopParams[0].(map[string]any)
	if p0["name"] != "task_id" || p0["required"] != true {
		t.Errorf("/ws/stop param = %v; want required task_id", p0)
	}
	if p0["x-ws-type"] != "uuid" {
		t.Errorf("/ws/stop task_id x-ws-type = %v; want uuid", p0["x-ws-type"])
	}
}

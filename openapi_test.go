package overseer

import (
	"encoding/json"
	"strings"
	"testing"
)

// helper: unmarshal spec, assert no error
func mustUnmarshalSpec(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("BuildOpenAPISpec returned invalid JSON: %v", err)
	}
	return spec
}

// helper: navigate spec["paths"]["/ws"]["ws"] → map
func wsOpFromSpec(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	wsPath, ok := paths["/ws"].(map[string]any)
	if !ok {
		t.Fatal("missing /ws path item")
	}
	wsOp, ok := wsPath["ws"].(map[string]any)
	if !ok {
		t.Fatal("missing ws operation on /ws path item")
	}
	return wsOp
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_structure
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_structure(t *testing.T) {
	spec := mustUnmarshalSpec(t, BuildOpenAPISpec(nil, "v1.2.3"))

	if got, _ := spec["swagger"].(string); got != "2.0" {
		t.Errorf(`swagger = %v; want "2.0"`, spec["swagger"])
	}
	info, _ := spec["info"].(map[string]any)
	if info["title"] != "sticky-overseer" {
		t.Errorf(`info.title = %v; want "sticky-overseer"`, info["title"])
	}
	if info["version"] != "v1.2.3" {
		t.Errorf(`info.version = %v; want "v1.2.3"`, info["version"])
	}

	// Exactly one path: /ws
	paths, _ := spec["paths"].(map[string]any)
	if len(paths) != 1 {
		t.Errorf("expected 1 path (/ws), got %d", len(paths))
	}

	wsOp := wsOpFromSpec(t, spec)

	// No protocol-specific fields on the connection descriptor
	for _, field := range []string{"type_field", "correlation_field", "x-websocket-endpoints"} {
		if _, exists := wsOp[field]; exists {
			t.Errorf("unexpected field %q on ws operation descriptor", field)
		}
	}

	// 12 standard protocol commands when no actions registered
	cmds, _ := wsOp["commands"].([]any)
	if len(cmds) != 12 {
		t.Errorf("expected 12 standard commands for nil actions, got %d", len(cmds))
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_actions
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
					"name":   nil,
					"suffix": {Default: &defVal, Validate: `value != ""`},
				},
			},
		},
		"echo": &stubHandler{info: ActionInfo{Name: "echo", Type: "exec"}},
	}

	spec := mustUnmarshalSpec(t, BuildOpenAPISpec(actions, "2.0.0"))

	// Still only /ws — no separate action paths
	paths, _ := spec["paths"].(map[string]any)
	if len(paths) != 1 {
		t.Fatalf("expected 1 path (/ws), got %d: %v", len(paths), paths)
	}

	wsOp := wsOpFromSpec(t, spec)
	cmds, _ := wsOp["commands"].([]any)
	// 12 standard + 2 actions = 14
	if len(cmds) != 14 {
		t.Fatalf("expected 14 commands (12 standard + 2 actions), got %d", len(cmds))
	}

	// Find the greet command by operationId
	var greetCmd map[string]any
	for _, c := range cmds {
		cmd := c.(map[string]any)
		if cmd["operationId"] == "start-greet" {
			greetCmd = cmd
			break
		}
	}
	if greetCmd == nil {
		t.Fatal("greet action command not found in commands")
	}

	if greetCmd["params_key"] != "params" {
		t.Errorf("greet params_key = %v; want params", greetCmd["params_key"])
	}
	if greetCmd["x-action-type"] != "exec" {
		t.Errorf("greet x-action-type = %v; want exec", greetCmd["x-action-type"])
	}

	// Parameters: hidden type + hidden action + name (required) + suffix (optional) = 4
	rawParams, _ := greetCmd["parameters"].([]any)
	if len(rawParams) != 4 {
		t.Fatalf("greet: expected 4 parameters (2 hidden + 2 visible), got %d", len(rawParams))
	}

	// First two are hidden (type, action)
	p0, _ := rawParams[0].(map[string]any)
	if p0["name"] != "type" || p0["hidden"] != true {
		t.Errorf("first param should be hidden type, got %v", p0)
	}
	if p0["default"] != "start" {
		t.Errorf("hidden type default = %v; want start", p0["default"])
	}
	p1, _ := rawParams[1].(map[string]any)
	if p1["name"] != "action" || p1["hidden"] != true {
		t.Errorf("second param should be hidden action, got %v", p1)
	}
	if p1["default"] != "greet" {
		t.Errorf("hidden action default = %v; want greet", p1["default"])
	}

	// Visible params: required "name" then optional "suffix"
	p2, _ := rawParams[2].(map[string]any)
	if p2["name"] != "name" || p2["required"] != true {
		t.Errorf("third param should be required name, got %v", p2)
	}
	p3, _ := rawParams[3].(map[string]any)
	if p3["name"] != "suffix" || p3["required"] == true {
		t.Errorf("fourth param should be optional suffix, got %v", p3)
	}
	if p3["default"] != defVal {
		t.Errorf("suffix default = %v; want %q", p3["default"], defVal)
	}
	if validate, _ := p3["validate"].(string); !strings.Contains(validate, "value") {
		t.Errorf("suffix validate = %v; expected CEL expression", p3["validate"])
	}

	// echo command: only the 2 hidden params (type + action), no visible params
	var echoCmd map[string]any
	for _, c := range cmds {
		cmd := c.(map[string]any)
		if cmd["operationId"] == "start-echo" {
			echoCmd = cmd
			break
		}
	}
	if echoCmd == nil {
		t.Fatal("echo action command not found")
	}
	echoParams, _ := echoCmd["parameters"].([]any)
	if len(echoParams) != 2 {
		t.Errorf("echo should have 2 hidden params (type+action), got %d", len(echoParams))
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_tags
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_tags(t *testing.T) {
	actions := map[string]ActionHandler{
		"beta":  &stubHandler{info: ActionInfo{Name: "beta", Type: "exec"}},
		"alpha": &stubHandler{info: ActionInfo{Name: "alpha", Type: "exec", Description: "Alpha action"}},
	}

	spec := mustUnmarshalSpec(t, BuildOpenAPISpec(actions, "1.0.0"))

	// Tags are no longer emitted — the spec should have no top-level tags array.
	if _, exists := spec["tags"]; exists {
		t.Errorf("expected no top-level tags, but found them: %v", spec["tags"])
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_extensions — x-task-pool, x-retry, x-dedupe-key
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

	spec := mustUnmarshalSpec(t, BuildOpenAPISpec(actions, "1.0.0"))
	wsOp := wsOpFromSpec(t, spec)
	cmds, _ := wsOp["commands"].([]any)

	var workerCmd map[string]any
	for _, c := range cmds {
		cmd := c.(map[string]any)
		if cmd["operationId"] == "start-worker" {
			workerCmd = cmd
			break
		}
	}
	if workerCmd == nil {
		t.Fatal("worker command not found")
	}

	pool, ok := workerCmd["x-task-pool"].(map[string]any)
	if !ok {
		t.Fatal("x-task-pool missing")
	}
	if pool["limit"].(float64) != 3 {
		t.Errorf("x-task-pool.limit = %v; want 3", pool["limit"])
	}

	dedupeKey, _ := workerCmd["x-dedupe-key"].([]any)
	if len(dedupeKey) != 1 || dedupeKey[0] != "url" {
		t.Errorf("x-dedupe-key = %v; want [url]", dedupeKey)
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_wsCommands — standard protocol commands
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_wsCommands(t *testing.T) {
	spec := mustUnmarshalSpec(t, BuildOpenAPISpec(nil, "1.0.0"))
	wsOp := wsOpFromSpec(t, spec)
	cmds, _ := wsOp["commands"].([]any)

	// Index commands by the hidden "type" param's default value.
	// Every protocol command carries a hidden(\"type\", <value>) param.
	byType := map[string]map[string]any{}
	for _, c := range cmds {
		cmd := c.(map[string]any)
		for _, p := range cmd["parameters"].([]any) {
			pm := p.(map[string]any)
			if pm["name"] == "type" && pm["hidden"] == true {
				if typVal, _ := pm["default"].(string); typVal != "" {
					byType[typVal] = cmd
				}
				break
			}
		}
	}

	expectedTypes := []string{
		"list", "stop", "reset", "replay", "describe",
		"pool_info", "purge", "set_pool", "subscribe",
		"unsubscribe", "metrics", "manifest",
	}
	for _, typ := range expectedTypes {
		if _, ok := byType[typ]; !ok {
			t.Errorf("missing command with hidden type=%q", typ)
		}
	}

	// stop: has required task_id (uuid) among its params
	stopCmd := byType["stop"]
	stopParams, _ := stopCmd["parameters"].([]any)
	var taskIDParam map[string]any
	for _, p := range stopParams {
		pm := p.(map[string]any)
		if pm["name"] == "task_id" {
			taskIDParam = pm
			break
		}
	}
	if taskIDParam == nil {
		t.Fatal("stop: task_id param not found")
	}
	if taskIDParam["required"] != true {
		t.Errorf("stop task_id required = %v; want true", taskIDParam["required"])
	}
	if taskIDParam["type"] != "uuid" {
		t.Errorf("stop task_id type = %v; want uuid", taskIDParam["type"])
	}

	// manifest: only hidden type + optional id (2 params, no action-specific visible params)
	manifestCmd := byType["manifest"]
	manifestParams, _ := manifestCmd["parameters"].([]any)
	if len(manifestParams) != 2 {
		t.Errorf("manifest: expected 2 params (hidden type + id), got %d", len(manifestParams))
	}

	// set_pool has object-typed excess param
	setPoolCmd := byType["set_pool"]
	setPoolParams, _ := setPoolCmd["parameters"].([]any)
	var excessParam map[string]any
	for _, p := range setPoolParams {
		pm := p.(map[string]any)
		if pm["name"] == "excess" {
			excessParam = pm
			break
		}
	}
	if excessParam == nil {
		t.Fatal("set_pool: excess param not found")
	}
	if excessParam["type"] != "object" {
		t.Errorf("set_pool excess type = %v; want object", excessParam["type"])
	}
}

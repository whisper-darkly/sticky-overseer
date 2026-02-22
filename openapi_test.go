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
	if wsOp["type_field"] != "type" {
		t.Errorf("ws.type_field = %v; want type", wsOp["type_field"])
	}
	if wsOp["correlation_field"] != "id" {
		t.Errorf("ws.correlation_field = %v; want id", wsOp["correlation_field"])
	}

	// 12 standard protocol commands when no actions registered
	cmds, _ := wsOp["commands"].([]any)
	if len(cmds) != 12 {
		t.Errorf("expected 12 standard commands for nil actions, got %d", len(cmds))
	}

	// No x-websocket-endpoints root extension
	if _, exists := spec["x-websocket-endpoints"]; exists {
		t.Error("unexpected x-websocket-endpoints in spec")
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

	// Find the greet command
	var greetCmd map[string]any
	for _, c := range cmds {
		cmd := c.(map[string]any)
		if cmd["action"] == "greet" {
			greetCmd = cmd
			break
		}
	}
	if greetCmd == nil {
		t.Fatal("greet action command not found in commands")
	}

	if greetCmd["message"] != "start" {
		t.Errorf("greet message = %v; want start", greetCmd["message"])
	}
	if greetCmd["operationId"] != "start-greet" {
		t.Errorf("greet operationId = %v; want start-greet", greetCmd["operationId"])
	}
	if greetCmd["params_key"] != "params" {
		t.Errorf("greet params_key = %v; want params", greetCmd["params_key"])
	}
	if greetCmd["x-action-type"] != "exec" {
		t.Errorf("greet x-action-type = %v; want exec", greetCmd["x-action-type"])
	}

	rawParams, _ := greetCmd["parameters"].([]any)
	if len(rawParams) != 2 {
		t.Fatalf("greet: expected 2 parameters, got %d", len(rawParams))
	}

	// Required param first
	p0, _ := rawParams[0].(map[string]any)
	if p0["name"] != "name" {
		t.Errorf("first param = %v; want name", p0["name"])
	}
	if p0["required"] != true {
		t.Errorf("name required = %v; want true", p0["required"])
	}
	// Type is "string" (default from ParamSpec)
	if p0["type"] != "string" {
		t.Errorf("name type = %v; want string", p0["type"])
	}

	p1, _ := rawParams[1].(map[string]any)
	if p1["name"] != "suffix" {
		t.Errorf("second param = %v; want suffix", p1["name"])
	}
	if p1["required"] == true {
		t.Errorf("suffix required = true; want false/omitted")
	}
	if p1["default"] != defVal {
		t.Errorf("suffix default = %v; want %q", p1["default"], defVal)
	}
	if validate, _ := p1["validate"].(string); !strings.Contains(validate, "value") {
		t.Errorf("suffix validate = %v; expected CEL expression", p1["validate"])
	}

	// echo command: no parameters
	var echoCmd map[string]any
	for _, c := range cmds {
		cmd := c.(map[string]any)
		if cmd["action"] == "echo" {
			echoCmd = cmd
			break
		}
	}
	if echoCmd == nil {
		t.Fatal("echo action command not found")
	}
	if echoCmd["parameters"] != nil {
		t.Errorf("echo should have no parameters, got %v", echoCmd["parameters"])
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

	tags, _ := spec["tags"].([]any)
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags (ws-commands + 2 actions), got %d", len(tags))
	}
	first, _ := tags[0].(map[string]any)
	if first["name"] != "ws-commands" {
		t.Errorf("first tag = %v; want ws-commands", first["name"])
	}
	second, _ := tags[1].(map[string]any)
	if second["name"] != "alpha" {
		t.Errorf("second tag = %v; want alpha", second["name"])
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
		if cmd["action"] == "worker" {
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

	// Index commands by message name
	byMessage := map[string]map[string]any{}
	for _, c := range cmds {
		cmd := c.(map[string]any)
		if msg, _ := cmd["message"].(string); msg != "" {
			byMessage[msg] = cmd
		}
	}

	expectedMessages := []string{
		"list", "stop", "reset", "replay", "describe",
		"pool_info", "purge", "set_pool", "subscribe",
		"unsubscribe", "metrics", "manifest",
	}
	for _, msg := range expectedMessages {
		if _, ok := byMessage[msg]; !ok {
			t.Errorf("missing command with message=%q", msg)
		}
	}

	// stop requires task_id (uuid)
	stopCmd := byMessage["stop"]
	stopParams, _ := stopCmd["parameters"].([]any)
	if len(stopParams) != 1 {
		t.Fatalf("stop: expected 1 param, got %d", len(stopParams))
	}
	p0, _ := stopParams[0].(map[string]any)
	if p0["name"] != "task_id" {
		t.Errorf("stop param name = %v; want task_id", p0["name"])
	}
	if p0["required"] != true {
		t.Errorf("stop task_id required = %v; want true", p0["required"])
	}
	if p0["type"] != "uuid" {
		t.Errorf("stop task_id type = %v; want uuid", p0["type"])
	}

	// manifest has no parameters
	manifestCmd := byMessage["manifest"]
	if manifestCmd["parameters"] != nil {
		t.Errorf("manifest should have no parameters, got %v", manifestCmd["parameters"])
	}

	// set_pool has object-typed excess param
	setPoolCmd := byMessage["set_pool"]
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

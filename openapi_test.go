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

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("paths field missing or wrong type")
	}
	if len(paths) != 0 {
		t.Errorf("expected 0 paths for nil actions, got %d", len(paths))
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
					"name":    nil, // required
					"suffix":  {Default: &defVal, Validate: `value != ""`},
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
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}

	// Verify greet path exists.
	greetPath, ok := paths["/actions/greet/start"].(map[string]any)
	if !ok {
		t.Fatal("missing /actions/greet/start path")
	}
	post, ok := greetPath["post"].(map[string]any)
	if !ok {
		t.Fatal("missing post operation for /actions/greet/start")
	}

	// Check operationId.
	if post["operationId"] != "start-greet" {
		t.Errorf("operationId = %v; want start-greet", post["operationId"])
	}

	// Check x-action-type.
	if post["x-action-type"] != "exec" {
		t.Errorf("x-action-type = %v; want exec", post["x-action-type"])
	}

	// Check parameters include both name (required) and suffix (optional).
	rawParams, _ := post["parameters"].([]any)
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
	echoPath, ok := paths["/actions/echo/start"].(map[string]any)
	if !ok {
		t.Fatal("missing /actions/echo/start path")
	}
	echoPost, _ := echoPath["post"].(map[string]any)
	echoParams := echoPost["parameters"]
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
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tags))
	}
	// Tags should be sorted alphabetically.
	first, _ := tags[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Errorf("first tag = %v; want alpha (alphabetical order)", first["name"])
	}
}

// ---------------------------------------------------------------------------
// TestBuildOpenAPISpec_extensions — verifies x-task-pool, x-retry, x-dedupe-key
// ---------------------------------------------------------------------------

func TestBuildOpenAPISpec_extensions(t *testing.T) {
	actions := map[string]ActionHandler{
		"worker": &stubHandler{
			info: ActionInfo{
				Name: "worker",
				Type: "exec",
				TaskPool: PoolConfig{Limit: 3},
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
	path, _ := paths["/actions/worker/start"].(map[string]any)
	post, _ := path["post"].(map[string]any)

	pool, ok := post["x-task-pool"].(map[string]any)
	if !ok {
		t.Fatal("x-task-pool extension missing")
	}
	if pool["limit"].(float64) != 3 {
		t.Errorf("x-task-pool.limit = %v; want 3", pool["limit"])
	}

	dedupeKey, _ := post["x-dedupe-key"].([]any)
	if len(dedupeKey) != 1 || dedupeKey[0] != "url" {
		t.Errorf("x-dedupe-key = %v; want [url]", dedupeKey)
	}
}

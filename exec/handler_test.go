package exec

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	overseer "github.com/whisper-darkly/sticky-overseer/v2"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeExecHandler(name string, cfg ExecHandlerConfig, retry overseer.RetryPolicy, pool overseer.PoolConfig) (*ExecHandler, error) {
	celPrograms := make(map[string]cel.Program, len(cfg.Parameters))
	for pname, spec := range cfg.Parameters {
		if spec == nil || spec.Validate == "" {
			continue
		}
		prog, err := overseer.CompileCELProgram(spec.Validate)
		if err != nil {
			return nil, err
		}
		if prog != nil {
			celPrograms[pname] = prog
		}
	}
	return &ExecHandler{
		name:        name,
		cfg:         cfg,
		mergedRetry: retry,
		poolCfg:     pool,
		celPrograms: celPrograms,
	}, nil
}

func ptrStr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// TestExecHandler_Describe
// ---------------------------------------------------------------------------

func TestExecHandler_Describe(t *testing.T) {
	defaultVal := "default"
	cfg := ExecHandlerConfig{
		Entrypoint: "/bin/echo",
		Parameters: map[string]*overseer.ParamSpec{
			"msg": {Default: &defaultVal},
		},
	}
	h, err := makeExecHandler("greet", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected error creating handler: %v", err)
	}

	info := h.Describe()

	if info.Type != "exec" {
		t.Errorf("expected Type=%q, got %q", "exec", info.Type)
	}
	if info.Name != "greet" {
		t.Errorf("expected Name=%q, got %q", "greet", info.Name)
	}
	if info.Params == nil {
		t.Error("expected Params to be non-nil even when empty")
	}
}

func TestExecHandler_Describe_EmptyParams(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "/bin/true",
		Parameters: map[string]*overseer.ParamSpec{},
	}
	h, err := makeExecHandler("noop", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info := h.Describe()
	if info.Params == nil {
		t.Error("Params must be non-nil (empty map), not nil")
	}
	if len(info.Params) != 0 {
		t.Errorf("expected 0 params, got %d", len(info.Params))
	}
}

// ---------------------------------------------------------------------------
// TestExecHandler_Validate
// ---------------------------------------------------------------------------

func TestExecHandler_Validate_Required(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "/bin/echo",
		Parameters: map[string]*overseer.ParamSpec{
			"msg": {Default: nil, Validate: ""},
		},
	}
	h, err := makeExecHandler("echo", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected handler creation error: %v", err)
	}

	err = h.Validate(map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing required parameter, got nil")
	}
	if !strings.Contains(err.Error(), "msg") {
		t.Errorf("error should mention the missing param name %q; got: %v", "msg", err)
	}
}

func TestExecHandler_Validate_Default(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "/bin/echo",
		Parameters: map[string]*overseer.ParamSpec{
			"greeting": {Default: ptrStr("hello"), Validate: ""},
		},
	}
	h, err := makeExecHandler("echo", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected handler creation error: %v", err)
	}

	if err := h.Validate(map[string]string{}); err != nil {
		t.Fatalf("unexpected validation error for param with default: %v", err)
	}

	resolved, err := h.validateAndApplyDefaults(map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error from validateAndApplyDefaults: %v", err)
	}
	if resolved["greeting"] != "hello" {
		t.Errorf("expected resolved[greeting]=%q, got %q", "hello", resolved["greeting"])
	}
}

func TestExecHandler_Validate_CEL_Pass(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "/bin/echo",
		Parameters: map[string]*overseer.ParamSpec{
			"env": {Default: nil, Validate: `value in ['dev', 'staging', 'prod']`},
		},
	}
	h, err := makeExecHandler("deploy", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected handler creation error: %v", err)
	}

	if err := h.Validate(map[string]string{"env": "prod"}); err != nil {
		t.Errorf("expected validation to pass for value %q, got error: %v", "prod", err)
	}
}

func TestExecHandler_Validate_CEL_Fail(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "/bin/echo",
		Parameters: map[string]*overseer.ParamSpec{
			"env": {Default: nil, Validate: `value in ['dev', 'staging', 'prod']`},
		},
	}
	h, err := makeExecHandler("deploy", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected handler creation error: %v", err)
	}

	err = h.Validate(map[string]string{"env": "canary"})
	if err == nil {
		t.Fatal("expected validation to fail for value not in allowed list, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestExecHandler_Start
// ---------------------------------------------------------------------------

func TestExecHandler_Start_Template(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "echo",
		Command:    []string{"[[.msg]]"},
		Parameters: map[string]*overseer.ParamSpec{
			"msg": {Default: nil},
		},
	}
	h, err := makeExecHandler("echo", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected handler creation error: %v", err)
	}

	done := make(chan struct{})
	cb := overseer.WorkerCallbacks{
		OnOutput: func(msg *overseer.OutputMessage) {},
		LogEvent: func(v any) {},
		OnExited: func(w *overseer.Worker, exitCode int, intentional bool, ts time.Time) {
			close(done)
		},
	}

	w, err := h.Start("task-1", map[string]string{"msg": "hello-world"}, cb)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil Worker from Start")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for worker to exit")
	}
}

func TestExecHandler_Start_TemplateRenderError(t *testing.T) {
	cfg := ExecHandlerConfig{
		Entrypoint: "echo",
		Command:    []string{"[[.unclosed"},
		Parameters: map[string]*overseer.ParamSpec{},
	}
	h, err := makeExecHandler("echo", cfg, overseer.RetryPolicy{}, overseer.PoolConfig{})
	if err != nil {
		t.Fatalf("unexpected handler creation error: %v", err)
	}

	cb := overseer.WorkerCallbacks{
		OnOutput: func(msg *overseer.OutputMessage) {},
		LogEvent: func(v any) {},
		OnExited: func(w *overseer.Worker, exitCode int, intentional bool, ts time.Time) {},
	}

	_, err = h.Start("task-2", map[string]string{}, cb)
	if err == nil {
		t.Fatal("expected error for malformed template, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestExecFactory_Create_Success
// ---------------------------------------------------------------------------

func TestExecFactory_Create_Success(t *testing.T) {
	f := &execHandlerFactory{}

	config := map[string]any{
		"entrypoint": "/usr/bin/env",
		"command":    []any{"echo", "[[.msg]]"},
		"parameters": map[string]any{
			"msg": map[string]any{"default": "world"},
		},
	}

	handler, err := f.Create(config, "greet", overseer.RetryPolicy{}, overseer.PoolConfig{}, nil)
	if err != nil {
		t.Fatalf("unexpected error from Create: %v", err)
	}
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}

	info := handler.Describe()
	if info.Type != "exec" {
		t.Errorf("expected Type=%q, got %q", "exec", info.Type)
	}
	if info.Name != "greet" {
		t.Errorf("expected Name=%q, got %q", "greet", info.Name)
	}
}

func TestExecFactory_BadCEL(t *testing.T) {
	f := &execHandlerFactory{}

	config := map[string]any{
		"entrypoint": "/bin/echo",
		"parameters": map[string]any{
			"level": map[string]any{
				"validate": `value in [`,
			},
		},
	}

	_, err := f.Create(config, "badcel", overseer.RetryPolicy{}, overseer.PoolConfig{}, nil)
	if err == nil {
		t.Fatal("expected error for invalid CEL expression at Create time, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestRenderTemplate
// ---------------------------------------------------------------------------

func TestRenderTemplate_Simple(t *testing.T) {
	got, err := renderTemplate("hello [[.name]]", map[string]string{"name": "world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", got)
	}
}

func TestRenderTemplate_NoPlaceholders(t *testing.T) {
	got, err := renderTemplate("/usr/bin/make", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/usr/bin/make" {
		t.Errorf("expected %q, got %q", "/usr/bin/make", got)
	}
}

func TestRenderTemplate_MultiplePlaceholders(t *testing.T) {
	got, err := renderTemplate("[[.a]]-[[.b]]-[[.c]]", map[string]string{"a": "1", "b": "2", "c": "3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "1-2-3" {
		t.Errorf("expected %q, got %q", "1-2-3", got)
	}
}

func TestRenderTemplate_ParseError(t *testing.T) {
	_, err := renderTemplate("[[.unclosed", map[string]string{})
	if err == nil {
		t.Fatal("expected parse error for malformed template, got nil")
	}
}

func TestRenderTemplate_ShellDollarUnaffected(t *testing.T) {
	got, err := renderTemplate("export FOO=$BAR", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "export FOO=$BAR" {
		t.Errorf("expected shell syntax to be preserved, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// TestExecFactory_TypeString
// ---------------------------------------------------------------------------

func TestExecFactory_TypeString(t *testing.T) {
	f := &execHandlerFactory{}
	if f.Type() != "exec" {
		t.Errorf("expected factory type %q, got %q", "exec", f.Type())
	}
}

// ---------------------------------------------------------------------------
// TestExecHandler_OutputFilter
// ---------------------------------------------------------------------------

func TestExecHandler_OutputFilter_Stdout(t *testing.T) {
	f := &execHandlerFactory{}

	config := map[string]any{
		"entrypoint": "/bin/sh",
		"command":    []any{"-c", "echo PASS_line1; echo FAIL_line; echo PASS_line2"},
		"output": map[string]any{
			"stdout": map[string]any{
				"condition": `output.data.contains("PASS")`,
			},
		},
	}

	handler, err := f.Create(config, "filter-test", overseer.RetryPolicy{}, overseer.PoolConfig{}, nil)
	if err != nil {
		t.Fatalf("unexpected error from Create: %v", err)
	}

	var mu sync.Mutex
	var capturedLines []string
	done := make(chan struct{})
	cb := overseer.WorkerCallbacks{
		OnOutput: func(msg *overseer.OutputMessage) {
			mu.Lock()
			capturedLines = append(capturedLines, strings.TrimSpace(msg.Data))
			mu.Unlock()
		},
		LogEvent: func(v any) {},
		OnExited: func(w *overseer.Worker, exitCode int, intentional bool, ts time.Time) {
			close(done)
		},
	}

	w, err := handler.Start("filter-task-1", map[string]string{}, cb)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil Worker")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for worker to exit")
	}
	time.Sleep(50 * time.Millisecond) // allow callbacks to flush

	mu.Lock()
	lines := make([]string, len(capturedLines))
	copy(lines, capturedLines)
	mu.Unlock()

	for _, line := range lines {
		if !strings.Contains(line, "PASS") {
			t.Errorf("unexpected line forwarded by filter: %q", line)
		}
	}
	if len(lines) != 2 {
		t.Errorf("expected 2 PASS lines, got %d: %v", len(lines), lines)
	}
}

func TestExecHandler_OutputFilter_EmptyCondition(t *testing.T) {
	f := &execHandlerFactory{}

	config := map[string]any{
		"entrypoint": "/bin/sh",
		"command":    []any{"-c", "echo line1; echo line2; echo line3"},
		"output": map[string]any{
			"stdout": map[string]any{
				"condition": "",
			},
		},
	}

	handler, err := f.Create(config, "nofilter-test", overseer.RetryPolicy{}, overseer.PoolConfig{}, nil)
	if err != nil {
		t.Fatalf("unexpected error from Create: %v", err)
	}

	var mu sync.Mutex
	var capturedLines []string
	done := make(chan struct{})
	cb := overseer.WorkerCallbacks{
		OnOutput: func(msg *overseer.OutputMessage) {
			mu.Lock()
			capturedLines = append(capturedLines, strings.TrimSpace(msg.Data))
			mu.Unlock()
		},
		LogEvent: func(v any) {},
		OnExited: func(w *overseer.Worker, exitCode int, intentional bool, ts time.Time) {
			close(done)
		},
	}

	_, err = handler.Start("filter-task-2", map[string]string{}, cb)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for worker to exit")
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	n := len(capturedLines)
	mu.Unlock()

	if n != 3 {
		t.Errorf("expected 3 output lines (no filter), got %d", n)
	}
}

func TestExecHandler_OutputFilter_BadCEL(t *testing.T) {
	f := &execHandlerFactory{}

	config := map[string]any{
		"entrypoint": "/bin/echo",
		"output": map[string]any{
			"stdout": map[string]any{
				"condition": `output.data.notAFunction(`,
			},
		},
	}

	_, err := f.Create(config, "badcel-output", overseer.RetryPolicy{}, overseer.PoolConfig{}, nil)
	if err == nil {
		t.Fatal("expected CEL compilation error at Create time, got nil")
	}
}

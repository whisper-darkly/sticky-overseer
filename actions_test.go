package main

import (
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Stub types for testing
// ---------------------------------------------------------------------------

// stubFactory is a minimal ActionHandlerFactory for testing the registry.
type stubFactory struct {
	typeName string
}

func (s *stubFactory) Type() string { return s.typeName }

func (s *stubFactory) Create(config map[string]any, actionName string, mergedRetry RetryPolicy, poolCfg PoolConfig, dedupeKey []string) (ActionHandler, error) {
	return &stubHandler{
		info: ActionInfo{
			Name:      actionName,
			Type:      s.typeName,
			DedupeKey: dedupeKey,
		},
	}, nil
}

// errorFactory always returns an error from Create.
type errorFactory struct {
	typeName string
}

func (e *errorFactory) Type() string { return e.typeName }

func (e *errorFactory) Create(config map[string]any, actionName string, mergedRetry RetryPolicy, poolCfg PoolConfig, dedupeKey []string) (ActionHandler, error) {
	return nil, errors.New("create failed intentionally")
}

// stubHandler is a minimal ActionHandler for testing buildActionHandlers.
type stubHandler struct {
	info ActionInfo
}

func (h *stubHandler) Describe() ActionInfo { return h.info }

func (h *stubHandler) Validate(params map[string]string) error { return nil }

func (h *stubHandler) Start(taskID string, params map[string]string, cb workerCallbacks) (*Worker, error) {
	return nil, errors.New("stubHandler.Start not implemented")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isolatedRegistry runs fn with a fresh, empty factoryRegistry and restores
// the original registry afterwards. This prevents test pollution.
func isolatedRegistry(t *testing.T, fn func()) {
	t.Helper()
	saved := factoryRegistry
	factoryRegistry = nil
	defer func() { factoryRegistry = saved }()
	fn()
}

// ---------------------------------------------------------------------------
// Factory registry tests
// ---------------------------------------------------------------------------

func TestRegisterFactory(t *testing.T) {
	isolatedRegistry(t, func() {
		f := &stubFactory{typeName: "alpha"}
		RegisterFactory(f)

		if len(factoryRegistry) != 1 {
			t.Fatalf("expected 1 factory, got %d", len(factoryRegistry))
		}
		if factoryRegistry[0].Type() != "alpha" {
			t.Errorf("expected type 'alpha', got %q", factoryRegistry[0].Type())
		}
	})
}

func TestRegisterFactory_Multiple(t *testing.T) {
	isolatedRegistry(t, func() {
		names := []string{"alpha", "beta", "gamma"}
		for _, n := range names {
			RegisterFactory(&stubFactory{typeName: n})
		}

		if len(factoryRegistry) != len(names) {
			t.Fatalf("expected %d factories, got %d", len(names), len(factoryRegistry))
		}
		for i, n := range names {
			if factoryRegistry[i].Type() != n {
				t.Errorf("index %d: expected %q, got %q", i, n, factoryRegistry[i].Type())
			}
		}
	})
}

// ---------------------------------------------------------------------------
// buildActionHandlers tests
// ---------------------------------------------------------------------------

func TestBuildActionHandlers_UnknownType(t *testing.T) {
	isolatedRegistry(t, func() {
		RegisterFactory(&stubFactory{typeName: "exec"})

		cfg := &Config{
			Actions: map[string]ActionConfig{
				"build": {Type: "nonexistent"},
			},
		}

		_, err := buildActionHandlers(cfg)
		if err == nil {
			t.Fatal("expected an error for unknown action type, got nil")
		}
	})
}

func TestBuildActionHandlers_Success(t *testing.T) {
	isolatedRegistry(t, func() {
		RegisterFactory(&stubFactory{typeName: "exec"})

		cfg := &Config{
			Actions: map[string]ActionConfig{
				"build": {Type: "exec"},
				"test":  {Type: "exec"},
			},
		}

		handlers, err := buildActionHandlers(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(handlers) != 2 {
			t.Fatalf("expected 2 handlers, got %d", len(handlers))
		}
		if _, ok := handlers["build"]; !ok {
			t.Error("expected handler 'build' to be present")
		}
		if _, ok := handlers["test"]; !ok {
			t.Error("expected handler 'test' to be present")
		}
	})
}

func TestBuildActionHandlers_CreateError(t *testing.T) {
	isolatedRegistry(t, func() {
		RegisterFactory(&errorFactory{typeName: "failing"})

		cfg := &Config{
			Actions: map[string]ActionConfig{
				"bad": {Type: "failing"},
			},
		}

		_, err := buildActionHandlers(cfg)
		if err == nil {
			t.Fatal("expected an error from Create, got nil")
		}
	})
}

func TestBuildActionHandlers_EmptyConfig(t *testing.T) {
	isolatedRegistry(t, func() {
		cfg := &Config{
			Actions: map[string]ActionConfig{},
		}

		handlers, err := buildActionHandlers(cfg)
		if err != nil {
			t.Fatalf("unexpected error for empty actions map: %v", err)
		}
		if len(handlers) != 0 {
			t.Errorf("expected empty handlers map, got %d entries", len(handlers))
		}
	})
}

// ---------------------------------------------------------------------------
// mergeRetryPolicy tests
// ---------------------------------------------------------------------------

func TestMergeRetryPolicy_GlobalDefaults(t *testing.T) {
	global := RetryPolicy{
		RestartDelay:   5 * time.Second,
		ErrorWindow:    60 * time.Second,
		ErrorThreshold: 3,
	}
	override := RetryPolicy{} // all zero values

	result := mergeRetryPolicy(global, override)

	if result.RestartDelay != global.RestartDelay {
		t.Errorf("RestartDelay: want %v, got %v", global.RestartDelay, result.RestartDelay)
	}
	if result.ErrorWindow != global.ErrorWindow {
		t.Errorf("ErrorWindow: want %v, got %v", global.ErrorWindow, result.ErrorWindow)
	}
	if result.ErrorThreshold != global.ErrorThreshold {
		t.Errorf("ErrorThreshold: want %d, got %d", global.ErrorThreshold, result.ErrorThreshold)
	}
}

func TestMergeRetryPolicy_OverrideNonZero(t *testing.T) {
	global := RetryPolicy{
		RestartDelay:   5 * time.Second,
		ErrorWindow:    60 * time.Second,
		ErrorThreshold: 3,
	}
	override := RetryPolicy{
		RestartDelay:   10 * time.Second,
		ErrorWindow:    120 * time.Second,
		ErrorThreshold: 5,
	}

	result := mergeRetryPolicy(global, override)

	if result.RestartDelay != override.RestartDelay {
		t.Errorf("RestartDelay: want %v, got %v", override.RestartDelay, result.RestartDelay)
	}
	if result.ErrorWindow != override.ErrorWindow {
		t.Errorf("ErrorWindow: want %v, got %v", override.ErrorWindow, result.ErrorWindow)
	}
	if result.ErrorThreshold != override.ErrorThreshold {
		t.Errorf("ErrorThreshold: want %d, got %d", override.ErrorThreshold, result.ErrorThreshold)
	}
}

func TestMergeRetryPolicy_Zero_Ignored(t *testing.T) {
	global := RetryPolicy{
		RestartDelay:   5 * time.Second,
		ErrorWindow:    60 * time.Second,
		ErrorThreshold: 3,
	}
	// override has only one non-zero field; the zeros must NOT override global
	override := RetryPolicy{
		RestartDelay:   0,            // zero — keep global
		ErrorWindow:    30 * time.Second, // non-zero — override
		ErrorThreshold: 0,            // zero — keep global
	}

	result := mergeRetryPolicy(global, override)

	if result.RestartDelay != global.RestartDelay {
		t.Errorf("RestartDelay: want global %v, got %v", global.RestartDelay, result.RestartDelay)
	}
	if result.ErrorWindow != override.ErrorWindow {
		t.Errorf("ErrorWindow: want override %v, got %v", override.ErrorWindow, result.ErrorWindow)
	}
	if result.ErrorThreshold != global.ErrorThreshold {
		t.Errorf("ErrorThreshold: want global %d, got %d", global.ErrorThreshold, result.ErrorThreshold)
	}
}

// ---------------------------------------------------------------------------
// CompileCELProgram tests
// ---------------------------------------------------------------------------

func TestCompileCELProgram_Valid(t *testing.T) {
	prog, err := CompileCELProgram(`value in ['foo', 'bar']`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prog == nil {
		t.Fatal("expected non-nil program for valid expression")
	}
}

func TestCompileCELProgram_Invalid(t *testing.T) {
	_, err := CompileCELProgram(`value in [`)
	if err == nil {
		t.Fatal("expected error for invalid CEL syntax, got nil")
	}
}

func TestCompileCELProgram_Empty(t *testing.T) {
	prog, err := CompileCELProgram("")
	if err != nil {
		t.Fatalf("expected nil error for empty expression, got: %v", err)
	}
	if prog != nil {
		t.Fatal("expected nil program for empty expression")
	}
}

// ---------------------------------------------------------------------------
// EvalCELBool tests
// ---------------------------------------------------------------------------

func TestEvalCELBool_Valid(t *testing.T) {
	tests := []struct {
		expr  string
		input string
		want  bool
	}{
		{`value == 'hello'`, "hello", true},
		{`value == 'hello'`, "world", false},
		{`value.startsWith('foo')`, "foobar", true},
		{`value.startsWith('foo')`, "barfoo", false},
		{`value in ['a', 'b', 'c']`, "b", true},
		{`value in ['a', 'b', 'c']`, "z", false},
	}

	for _, tc := range tests {
		prog, err := CompileCELProgram(tc.expr)
		if err != nil {
			t.Errorf("compile(%q): unexpected error: %v", tc.expr, err)
			continue
		}

		got, err := EvalCELBool(prog, tc.input)
		if err != nil {
			t.Errorf("eval(%q, %q): unexpected error: %v", tc.expr, tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("eval(%q, %q): want %v, got %v", tc.expr, tc.input, tc.want, got)
		}
	}
}

func TestEvalCELBool_NilProgram(t *testing.T) {
	// nil program means "no validation" — always returns true
	got, err := EvalCELBool(nil, "anything")
	if err != nil {
		t.Fatalf("unexpected error for nil program: %v", err)
	}
	if !got {
		t.Error("expected true for nil program (no validation)")
	}
}

// ---------------------------------------------------------------------------
// CompileOutputCELProgram tests
// ---------------------------------------------------------------------------

func TestCompileOutputCELProgram_Valid(t *testing.T) {
	prog, err := CompileOutputCELProgram(`output['stream'] == 'stdout'`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prog == nil {
		t.Fatal("expected non-nil program for valid expression")
	}
}

func TestCompileOutputCELProgram_Empty(t *testing.T) {
	prog, err := CompileOutputCELProgram("")
	if err != nil {
		t.Fatalf("expected nil error for empty expression, got: %v", err)
	}
	if prog != nil {
		t.Fatal("expected nil program for empty expression")
	}
}

func TestCompileOutputCELProgram_DataContains(t *testing.T) {
	prog, err := CompileOutputCELProgram(`output['stream'] == 'stdout' && output['data'].contains('ERROR')`)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	// Should match stdout with ERROR
	got, err := EvalOutputCELBool(prog, OutputContext{
		Stream: "stdout",
		Data:   "ERROR: something went wrong",
	})
	if err != nil {
		t.Fatalf("unexpected eval error: %v", err)
	}
	if !got {
		t.Error("expected true for stdout with ERROR data")
	}

	// Should not match stderr
	got, err = EvalOutputCELBool(prog, OutputContext{
		Stream: "stderr",
		Data:   "ERROR: something went wrong",
	})
	if err != nil {
		t.Fatalf("unexpected eval error: %v", err)
	}
	if got {
		t.Error("expected false for stderr even with ERROR data")
	}
}

func TestCompileOutputCELProgram_JSON(t *testing.T) {
	prog, err := CompileOutputCELProgram(`output['stream'] == 'stdout'`)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	// Provide explicit JSON map in OutputContext
	ctx := OutputContext{
		Stream: "stdout",
		Data:   `{"level":"error","msg":"bad thing"}`,
		JSON:   map[string]any{"level": "error", "msg": "bad thing"},
	}
	got, err := EvalOutputCELBool(prog, ctx)
	if err != nil {
		t.Fatalf("unexpected eval error: %v", err)
	}
	if !got {
		t.Error("expected true for stdout with JSON context")
	}
}

func TestEvalOutputCELBool_NilProgram(t *testing.T) {
	// nil program means no filter — always pass
	got, err := EvalOutputCELBool(nil, OutputContext{Stream: "stderr", Data: "whatever"})
	if err != nil {
		t.Fatalf("unexpected error for nil program: %v", err)
	}
	if !got {
		t.Error("expected true for nil output program")
	}
}

func TestEvalOutputCELBool_JSONAutoparse(t *testing.T) {
	// When ctx.JSON is nil, EvalOutputCELBool should auto-parse JSON from Data.
	// We just verify it does not error for valid JSON data.
	prog, err := CompileOutputCELProgram(`output['stream'] == 'stdout'`)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	ctx := OutputContext{
		Stream: "stdout",
		Data:   `{"key":"value"}`,
		// JSON is nil — should be auto-parsed internally
	}
	got, err := EvalOutputCELBool(prog, ctx)
	if err != nil {
		t.Fatalf("unexpected eval error: %v", err)
	}
	if !got {
		t.Error("expected true for stdout with auto-parsed JSON")
	}
}

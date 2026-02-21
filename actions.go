package overseer

import (
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"
)

// ActionHandler is the core abstraction for all action drivers. Each named
// action registered in the config must be backed by an ActionHandler.
type ActionHandler interface {
	// Describe returns metadata about this handler for introspection.
	Describe() ActionInfo

	// Validate checks that params satisfy the handler's requirements.
	// Returns nil if params are valid; returns an error describing the
	// first validation failure. CEL-based validation is performed here
	// so errors are caught before Start is ever called.
	Validate(params map[string]string) error

	// Start launches a worker for the given taskID with the supplied
	// params. The WorkerCallbacks are injected to decouple the worker
	// from the hub. Returns the running Worker or an error.
	Start(taskID string, params map[string]string, cb WorkerCallbacks) (*Worker, error)
}

// ActionHandlerFactory creates ActionHandler instances from configuration.
// Implementations are registered at init() time via RegisterFactory.
type ActionHandlerFactory interface {
	// Type returns the short driver name (e.g. "exec"). Must be unique
	// across all registered factories.
	Type() string

	// Create instantiates a handler for the named action using the
	// provided driver-specific config map, merged retry policy, pool
	// config, and dedupe key. Returns an error if the config is invalid.
	Create(config map[string]any, actionName string, mergedRetry RetryPolicy, poolCfg PoolConfig, dedupeKey []string) (ActionHandler, error)
}

// ActionInfo is the JSON-serialisable description of a named action,
// returned by ActionHandler.Describe() and included in ActionsMessage.
type ActionInfo struct {
	// Name is the action key from the config (e.g. "build"). Set by the
	// hub when building the handler map, not by the driver itself.
	Name string `json:"name"`

	// Type is the factory/driver type string (e.g. "exec").
	Type string `json:"type"`

	// Description is a human-readable summary of what the action does.
	Description string `json:"description,omitempty"`

	// Params describes accepted parameters. nil means the driver does not
	// expose parameter metadata.
	Params map[string]*ParamSpec `json:"params,omitempty"`

	// TaskPool is the effective pool configuration for this action.
	TaskPool PoolConfig `json:"task_pool"`

	// Retry is the effective retry policy for this action. nil means no
	// automatic restarts.
	Retry *RetryPolicy `json:"retry,omitempty"`

	// DedupeKey lists the parameter names that form the unique identity of a task
	// instance. When non-empty, a second start with the same parameter values for
	// these keys is rejected as a duplicate. nil/empty = deduplication inactive.
	DedupeKey []string `json:"dedupe_key,omitempty"`
}

// factoryRegistry holds all registered ActionHandlerFactory implementations.
// It is populated at init() time and read-only afterwards; no mutex required.
var factoryRegistry []ActionHandlerFactory

// RegisterFactory registers an ActionHandlerFactory so that BuildActionHandlers
// can look it up by type name. Must be called from init() functions only —
// access is not thread-safe.
func RegisterFactory(f ActionHandlerFactory) {
	factoryRegistry = append(factoryRegistry, f)
}

// BuildActionHandlers instantiates one ActionHandler per entry in cfg.Actions.
// Returns an error if any action references an unknown factory type or if the
// factory's Create call fails.
func BuildActionHandlers(cfg *Config) (map[string]ActionHandler, error) {
	result := make(map[string]ActionHandler, len(cfg.Actions))

	for name, actionCfg := range cfg.Actions {
		// Locate the factory for this action type.
		var factory ActionHandlerFactory
		for _, f := range factoryRegistry {
			if f.Type() == actionCfg.Type {
				factory = f
				break
			}
		}
		if factory == nil {
			return nil, fmt.Errorf("action %q: unknown action type %q (registered: %v)", name, actionCfg.Type, registeredTypes())
		}

		// Merge global retry policy with the per-action override.
		merged := mergeRetryPolicy(cfg.Retry, actionCfg.Retry)

		handler, err := factory.Create(actionCfg.Config, name, merged, actionCfg.TaskPool, actionCfg.DedupeKey)
		if err != nil {
			return nil, fmt.Errorf("action %q: failed to create handler: %w", name, err)
		}

		result[name] = handler
	}

	return result, nil
}

// registeredTypes returns a slice of all registered factory type strings,
// used for human-readable error messages.
func registeredTypes() []string {
	types := make([]string, len(factoryRegistry))
	for i, f := range factoryRegistry {
		types[i] = f.Type()
	}
	return types
}

// mergeRetryPolicy returns a RetryPolicy that starts with global as the
// baseline and applies non-zero fields from override. Zero values in override
// mean "not specified" and do NOT replace the global default.
func mergeRetryPolicy(global, override RetryPolicy) RetryPolicy {
	result := global
	if override.RestartDelay != 0 {
		result.RestartDelay = override.RestartDelay
	}
	if override.ErrorWindow != 0 {
		result.ErrorWindow = override.ErrorWindow
	}
	if override.ErrorThreshold != 0 {
		result.ErrorThreshold = override.ErrorThreshold
	}
	return result
}

// ListHandlers prints the type string of every registered factory to stdout,
// one per line. Called early in main when --list-handlers is passed; the
// process exits immediately after without opening the DB or starting the hub.
func ListHandlers() {
	for _, factory := range factoryRegistry {
		fmt.Println(factory.Type())
	}
}

// ---------------------------------------------------------------------------
// CEL compilation and evaluation helpers
// ---------------------------------------------------------------------------

// paramCELEnv is the shared CEL environment for parameter validation.
// Variables: "value" (string).
var paramCELEnv *cel.Env

func init() {
	var err error
	paramCELEnv, err = cel.NewEnv(
		cel.Variable("value", cel.StringType),
	)
	if err != nil {
		panic(fmt.Sprintf("actions: failed to create CEL parameter environment: %v", err))
	}
}

// CompileCELProgram compiles a CEL expression that operates on a single string
// variable named "value". Returns (nil, nil) when expr is empty, signalling
// "no validation". Returns an error for any syntax or type-check failure.
func CompileCELProgram(expr string) (cel.Program, error) {
	if expr == "" {
		return nil, nil
	}

	ast, issues := paramCELEnv.Compile(expr)
	if issues.Err() != nil {
		return nil, fmt.Errorf("CEL compile error: %w", issues.Err())
	}

	prog, err := paramCELEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("CEL program error: %w", err)
	}

	return prog, nil
}

// EvalCELBool evaluates a compiled CEL program with value=input and returns
// the boolean result. If prog is nil (no validation expression), it returns
// (true, nil) — meaning the value is always valid.
func EvalCELBool(prog cel.Program, input string) (bool, error) {
	if prog == nil {
		return true, nil
	}

	out, _, err := prog.Eval(map[string]any{
		"value": input,
	})
	if err != nil {
		return false, fmt.Errorf("CEL eval error: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL expression did not evaluate to bool (got %T)", out.Value())
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Output context CEL environment
// ---------------------------------------------------------------------------

// OutputContext holds the variables available to output-filter CEL expressions.
type OutputContext struct {
	// Stream is "stdout" or "stderr".
	Stream string

	// Data is the raw text of the output line.
	Data string

	// JSON is the parsed JSON object when Data is valid JSON; nil otherwise.
	JSON map[string]any
}

// outputCELEnv is the shared CEL environment for output filtering.
// Variables:
//   - output.stream (string)
//   - output.data   (string)
//   - output.json   (map[string, dyn])
var outputCELEnv *cel.Env

func init() {
	var err error
	outputCELEnv, err = cel.NewEnv(
		cel.Variable("output", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		panic(fmt.Sprintf("actions: failed to create CEL output environment: %v", err))
	}
}

// CompileOutputCELProgram compiles a CEL expression that can reference
// output.stream, output.data, and output.json (a map). Returns (nil, nil)
// when expr is empty. Returns an error for any syntax or type-check failure.
func CompileOutputCELProgram(expr string) (cel.Program, error) {
	if expr == "" {
		return nil, nil
	}

	ast, issues := outputCELEnv.Compile(expr)
	if issues.Err() != nil {
		return nil, fmt.Errorf("CEL output compile error: %w", issues.Err())
	}

	prog, err := outputCELEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("CEL output program error: %w", err)
	}

	return prog, nil
}

// EvalOutputCELBool evaluates a compiled output-filter CEL program with an
// OutputContext. Returns (true, nil) when prog is nil (no filter = always pass).
func EvalOutputCELBool(prog cel.Program, ctx OutputContext) (bool, error) {
	if prog == nil {
		return true, nil
	}

	// Build the "output" map. JSON is included only when Data is valid JSON.
	outputMap := map[string]any{
		"stream": ctx.Stream,
		"data":   ctx.Data,
		"json":   map[string]any{},
	}
	if ctx.JSON != nil {
		outputMap["json"] = ctx.JSON
	} else {
		// Attempt to parse Data as JSON for convenience.
		var parsed map[string]any
		if json.Unmarshal([]byte(ctx.Data), &parsed) == nil {
			outputMap["json"] = parsed
		}
	}

	out, _, err := prog.Eval(map[string]any{
		"output": outputMap,
	})
	if err != nil {
		return false, fmt.Errorf("CEL output eval error: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL output expression did not evaluate to bool (got %T)", out.Value())
	}

	return result, nil
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"

	"github.com/google/cel-go/cel"
)

// ---------------------------------------------------------------------------
// ExecHandlerConfig — driver-level configuration for the "exec" action type
// ---------------------------------------------------------------------------

// ExecHandlerConfig holds the configuration for a single exec action handler.
// It is populated by JSON-marshalling the raw map[string]any from ActionConfig.Config
// and unmarshalling it into this struct (no new dependencies required).
type ExecHandlerConfig struct {
	// Entrypoint is the executable to run. Supports [[ ]] template syntax.
	Entrypoint string `json:"entrypoint"`

	// Command is the argument list passed to Entrypoint. Each element supports
	// [[ ]] template syntax with the resolved parameter map as data.
	Command []string `json:"command"`

	// Parameters describes the accepted parameters for this action.
	// The map key is the parameter name; the value is a ParamSpec that
	// controls defaults and CEL-based validation.
	Parameters map[string]*ParamSpec `json:"parameters"`

	// Output defines per-stream output filtering rules using CEL expressions.
	// Example: output.stdout.condition: "output.data.contains('ERROR')"
	Output ExecOutputConfig `json:"output,omitempty"`
}

// ExecOutputConfig holds per-stream output filter configuration.
type ExecOutputConfig struct {
	// Stdout filters output lines from the task's stdout stream.
	// Condition is a CEL expression (empty = always forward).
	// Available context: output.stream (string), output.data (string), output.json (map).
	Stdout OutputRule `json:"stdout,omitempty"`

	// Stderr filters output lines from the task's stderr stream.
	// Same CEL expression rules as Stdout.
	Stderr OutputRule `json:"stderr,omitempty"`
}

// OutputRule controls whether an output line is forwarded to clients.
// Condition is a CEL expression evaluated against the output line context.
// Available variables: output.stream (string), output.data (string), output.json (map).
// Empty Condition means always forward.
type OutputRule struct {
	Condition string      `json:"condition,omitempty"`
	prog      cel.Program // compiled at Create time; nil = always forward
}

// ---------------------------------------------------------------------------
// ExecHandler — implements ActionHandler for the "exec" driver type
// ---------------------------------------------------------------------------

// ExecHandler executes OS processes via startWorker. It holds pre-compiled CEL
// programs (one per parameter with a Validate expression) so that any
// compilation errors surface at handler creation time, not at task start time.
type ExecHandler struct {
	// name is the action key from the config (e.g. "build").
	name string

	// cfg is the parsed ExecHandlerConfig.
	cfg ExecHandlerConfig

	// mergedRetry is the effective retry policy (global merged with per-action).
	mergedRetry RetryPolicy

	// poolCfg is the effective pool configuration for this action.
	poolCfg PoolConfig

	// dedupeKey is the list of parameter names forming the unique task identity.
	// Empty means deduplication is inactive for this action.
	dedupeKey []string

	// celPrograms maps parameter name → compiled CEL program.
	// Only populated for parameters that have a non-empty Validate expression.
	celPrograms map[string]cel.Program
}

// Describe returns ActionInfo metadata for this handler.
// Params is always non-nil (may be an empty map) to signal that the driver
// exposes parameter metadata.
func (h *ExecHandler) Describe() ActionInfo {
	// Return a copy of the Parameters map so callers cannot mutate internal state.
	params := make(map[string]*ParamSpec, len(h.cfg.Parameters))
	for k, v := range h.cfg.Parameters {
		params[k] = v
	}

	retry := h.mergedRetry // copy

	return ActionInfo{
		Name:      h.name,
		Type:      "exec",
		Params:    params,
		TaskPool:  h.poolCfg,
		Retry:     &retry,
		DedupeKey: h.dedupeKey,
	}
}

// Validate checks that params satisfies all parameter specifications:
//   - Required parameters (Default == nil) must be present.
//   - Missing optional parameters get their default value applied (but
//     Validate does NOT return those — callers must call Start which
//     calls validateAndApplyDefaults internally).
//   - Parameters with a non-empty Validate CEL expression are evaluated;
//     a false result means validation failed.
//
// The interface contract (from ActionHandler) returns only error; the
// caller is responsible for obtaining the defaults-applied map via Start.
func (h *ExecHandler) Validate(params map[string]string) error {
	_, err := h.validateAndApplyDefaults(params)
	return err
}

// validateAndApplyDefaults performs the full validation pass and returns a
// new map with defaults applied. This is used internally by both Validate and
// Start so that defaults are always present when templates are rendered.
func (h *ExecHandler) validateAndApplyDefaults(params map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(params))
	for k, v := range params {
		result[k] = v
	}

	for name, spec := range h.cfg.Parameters {
		val, present := result[name]

		if !present {
			// spec may be nil when the YAML/JSON config sets the parameter to
			// null, which means "required parameter with no default/validation".
			if spec == nil || spec.Default == nil {
				// Required parameter is missing.
				return nil, fmt.Errorf("exec: required parameter %q is missing", name)
			}
			// Optional parameter — apply its default.
			result[name] = *spec.Default
			val = *spec.Default
		}

		// Evaluate CEL validation expression if one was compiled.
		prog, hasProg := h.celPrograms[name]
		if hasProg && prog != nil {
			ok, err := EvalCELBool(prog, val)
			if err != nil {
				return nil, fmt.Errorf("exec: CEL evaluation error for parameter %q: %w", name, err)
			}
			if !ok {
				return nil, fmt.Errorf("exec: parameter %q failed validation (value: %q)", name, val)
			}
		}
	}

	return result, nil
}

// Start validates params, renders the Entrypoint and Command templates, then
// launches a worker via startWorker. Returns the running *Worker or an error.
//
// Template rendering uses Go's text/template with custom delimiters [[ and ]]
// to avoid conflicts with shell syntax. The resolved parameter map (with
// defaults applied) is passed as the template data.
func (h *ExecHandler) Start(taskID string, params map[string]string, cb workerCallbacks) (*Worker, error) {
	// Apply defaults and validate all parameters first.
	resolved, err := h.validateAndApplyDefaults(params)
	if err != nil {
		return nil, err
	}

	// Render the Entrypoint template.
	entrypoint, err := renderTemplate(h.cfg.Entrypoint, resolved)
	if err != nil {
		return nil, fmt.Errorf("exec: failed to render entrypoint template: %w", err)
	}
	if entrypoint == "" {
		return nil, fmt.Errorf("exec: entrypoint must not be empty after template rendering")
	}

	// Render each argument template.
	renderedArgs := make([]string, len(h.cfg.Command))
	for i, arg := range h.cfg.Command {
		rendered, err := renderTemplate(arg, resolved)
		if err != nil {
			return nil, fmt.Errorf("exec: failed to render command[%d] template: %w", i, err)
		}
		renderedArgs[i] = rendered
	}

	// Wrap the onOutput callback with per-stream CEL filtering.
	// Both IncludeStdout and IncludeStderr remain true so pipes are always
	// drained, preventing child process blocking on a full output buffer.
	filteredCB := cb
	if h.cfg.Output.Stdout.prog != nil || h.cfg.Output.Stderr.prog != nil {
		originalOnOutput := cb.onOutput
		filteredCB.onOutput = func(msg *OutputMessage) {
			rule := h.cfg.Output.Stdout
			if msg.Stream == StreamStderr {
				rule = h.cfg.Output.Stderr
			}
			if rule.prog != nil {
				ctx := OutputContext{
					Stream: string(msg.Stream),
					Data:   msg.Data,
				}
				pass, err := EvalOutputCELBool(rule.prog, ctx)
				if err != nil || !pass {
					return // Drop this line — filter did not pass
				}
			}
			originalOnOutput(msg)
		}
	}

	cfg := workerConfig{
		TaskID:        taskID,
		Command:       entrypoint,
		Args:          renderedArgs,
		IncludeStdout: true, // Always drain to prevent child process blocking
		IncludeStderr: true, // Always drain to prevent child process blocking
	}

	return startWorker(cfg, filteredCB)
}

// ---------------------------------------------------------------------------
// execHandlerFactory — registers the "exec" driver at init() time
// ---------------------------------------------------------------------------

type execHandlerFactory struct{}

// Type returns the driver name used to match ActionConfig.Type in YAML config.
func (f *execHandlerFactory) Type() string { return "exec" }

// Create instantiates an ExecHandler from the raw config map. All CEL
// expressions are compiled here so that any invalid expressions cause a
// startup error (fail-fast principle) rather than failing when a task starts.
func (f *execHandlerFactory) Create(config map[string]any, actionName string, mergedRetry RetryPolicy, poolCfg PoolConfig, dedupeKey []string) (ActionHandler, error) {
	// Convert the generic map to ExecHandlerConfig via JSON round-trip.
	// This is idiomatic Go and avoids reflection complexity; the json package
	// handles type coercions for us.
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("exec: failed to marshal config: %w", err)
	}

	var cfg ExecHandlerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("exec: failed to parse config: %w", err)
	}

	// Compile CEL programs for all parameter Validate expressions.
	// This happens once at handler creation (startup) — any bad expression
	// is a hard error, not a silent no-op.
	celPrograms := make(map[string]cel.Program, len(cfg.Parameters))
	for name, spec := range cfg.Parameters {
		if spec == nil || spec.Validate == "" {
			continue
		}
		prog, err := CompileCELProgram(spec.Validate)
		if err != nil {
			return nil, fmt.Errorf("exec: action %q parameter %q: CEL compile error: %w", actionName, name, err)
		}
		if prog != nil {
			celPrograms[name] = prog
		}
	}

	// Compile output filter CEL programs at creation time so any invalid
	// expressions are caught immediately (fail-fast).
	if cfg.Output.Stdout.Condition != "" {
		prog, err := CompileOutputCELProgram(cfg.Output.Stdout.Condition)
		if err != nil {
			return nil, fmt.Errorf("exec: action %q stdout filter CEL error: %w", actionName, err)
		}
		cfg.Output.Stdout.prog = prog
	}

	if cfg.Output.Stderr.Condition != "" {
		prog, err := CompileOutputCELProgram(cfg.Output.Stderr.Condition)
		if err != nil {
			return nil, fmt.Errorf("exec: action %q stderr filter CEL error: %w", actionName, err)
		}
		cfg.Output.Stderr.prog = prog
	}

	return &ExecHandler{
		name:        actionName,
		cfg:         cfg,
		mergedRetry: mergedRetry,
		poolCfg:     poolCfg,
		dedupeKey:   dedupeKey,
		celPrograms: celPrograms,
	}, nil
}

// init registers the execHandlerFactory so that buildActionHandlers can
// look it up when constructing handlers for actions of type "exec".
func init() {
	RegisterFactory(&execHandlerFactory{})
}

// ---------------------------------------------------------------------------
// renderTemplate — helper for [[ ]] delimited text/template rendering
// ---------------------------------------------------------------------------

// renderTemplate executes a Go text/template string using custom delimiters
// [[ and ]] (instead of the default {{ and }}) so that template syntax does not
// conflict with shell variable syntax (e.g. ${VAR}), JSON notation, or command braces.
//
// The data map is passed directly as the template dot value; parameter values
// are accessed as [[.paramName]] in the template string.
func renderTemplate(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("").Delims("[[", "]]").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("template parse error: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template execute error: %w", err)
	}
	return buf.String(), nil
}

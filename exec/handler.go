package exec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"

	"github.com/google/cel-go/cel"
	overseer "github.com/whisper-darkly/sticky-overseer/v2"
)

// ---------------------------------------------------------------------------
// ExecHandlerConfig — driver-level configuration for the "exec" action type
// ---------------------------------------------------------------------------

// ExecHandlerConfig holds the configuration for a single exec action handler.
type ExecHandlerConfig struct {
	// Entrypoint is the executable to run. Supports [[ ]] template syntax.
	Entrypoint string `json:"entrypoint"`

	// Command is the argument list passed to Entrypoint. Each element supports
	// [[ ]] template syntax with the resolved parameter map as data.
	Command []string `json:"command"`

	// Parameters describes the accepted parameters for this action.
	Parameters map[string]*overseer.ParamSpec `json:"parameters"`

	// Output defines per-stream output filtering rules using CEL expressions.
	Output ExecOutputConfig `json:"output,omitempty"`
}

// ExecOutputConfig holds per-stream output filter configuration.
type ExecOutputConfig struct {
	Stdout OutputRule `json:"stdout,omitempty"`
	Stderr OutputRule `json:"stderr,omitempty"`
}

// OutputRule controls whether an output line is forwarded to clients.
type OutputRule struct {
	Condition string      `json:"condition,omitempty"`
	prog      cel.Program // compiled at Create time; nil = always forward
}

// ---------------------------------------------------------------------------
// ExecHandler — implements overseer.ActionHandler for the "exec" driver type
// ---------------------------------------------------------------------------

// ExecHandler executes OS processes via overseer.StartWorker.
type ExecHandler struct {
	name        string
	cfg         ExecHandlerConfig
	mergedRetry overseer.RetryPolicy
	poolCfg     overseer.PoolConfig
	dedupeKey   []string
	celPrograms map[string]cel.Program
}

// Describe returns overseer.ActionInfo metadata for this handler.
func (h *ExecHandler) Describe() overseer.ActionInfo {
	params := make(map[string]*overseer.ParamSpec, len(h.cfg.Parameters))
	for k, v := range h.cfg.Parameters {
		params[k] = v
	}
	retry := h.mergedRetry
	return overseer.ActionInfo{
		Name:      h.name,
		Type:      "exec",
		Params:    params,
		TaskPool:  h.poolCfg,
		Retry:     &retry,
		DedupeKey: h.dedupeKey,
	}
}

// Validate checks that params satisfies all parameter specifications.
func (h *ExecHandler) Validate(params map[string]string) error {
	_, err := h.validateAndApplyDefaults(params)
	return err
}

// validateAndApplyDefaults performs the full validation pass and returns a
// new map with defaults applied.
func (h *ExecHandler) validateAndApplyDefaults(params map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(params))
	for k, v := range params {
		result[k] = v
	}

	for name, spec := range h.cfg.Parameters {
		val, present := result[name]

		if !present {
			if spec == nil || spec.Default == nil {
				return nil, fmt.Errorf("exec: required parameter %q is missing", name)
			}
			result[name] = *spec.Default
			val = *spec.Default
		}

		prog, hasProg := h.celPrograms[name]
		if hasProg && prog != nil {
			ok, err := overseer.EvalCELBool(prog, val)
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

// Start validates params, renders templates, then launches a worker.
func (h *ExecHandler) Start(taskID string, params map[string]string, cb overseer.WorkerCallbacks) (*overseer.Worker, error) {
	resolved, err := h.validateAndApplyDefaults(params)
	if err != nil {
		return nil, err
	}

	entrypoint, err := renderTemplate(h.cfg.Entrypoint, resolved)
	if err != nil {
		return nil, fmt.Errorf("exec: failed to render entrypoint template: %w", err)
	}
	if entrypoint == "" {
		return nil, fmt.Errorf("exec: entrypoint must not be empty after template rendering")
	}

	renderedArgs := make([]string, len(h.cfg.Command))
	for i, arg := range h.cfg.Command {
		rendered, err := renderTemplate(arg, resolved)
		if err != nil {
			return nil, fmt.Errorf("exec: failed to render command[%d] template: %w", i, err)
		}
		renderedArgs[i] = rendered
	}

	// Wrap the OnOutput callback with per-stream CEL filtering.
	filteredCB := cb
	if h.cfg.Output.Stdout.prog != nil || h.cfg.Output.Stderr.prog != nil {
		originalOnOutput := cb.OnOutput
		filteredCB = overseer.NewWorkerCallbacks(
			func(msg *overseer.OutputMessage) {
				rule := h.cfg.Output.Stdout
				if msg.Stream == overseer.StreamStderr {
					rule = h.cfg.Output.Stderr
				}
				if rule.prog != nil {
					ctx := overseer.OutputContext{
						Stream: string(msg.Stream),
						Data:   msg.Data,
					}
					pass, err := overseer.EvalOutputCELBool(rule.prog, ctx)
					if err != nil || !pass {
						return
					}
				}
				originalOnOutput(msg)
			},
			cb.LogEvent,
			cb.OnExited,
		)
	}

	cfg := overseer.WorkerConfig{
		TaskID:        taskID,
		Command:       entrypoint,
		Args:          renderedArgs,
		IncludeStdout: true,
		IncludeStderr: true,
	}

	return overseer.StartWorker(cfg, filteredCB)
}

// ---------------------------------------------------------------------------
// execHandlerFactory — registers the "exec" driver at init() time
// ---------------------------------------------------------------------------

type execHandlerFactory struct{}

func (f *execHandlerFactory) Type() string { return "exec" }

// Create instantiates an ExecHandler from the raw config map.
func (f *execHandlerFactory) Create(config map[string]any, actionName string, mergedRetry overseer.RetryPolicy, poolCfg overseer.PoolConfig, dedupeKey []string) (overseer.ActionHandler, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("exec: failed to marshal config: %w", err)
	}

	var cfg ExecHandlerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("exec: failed to parse config: %w", err)
	}

	celPrograms := make(map[string]cel.Program, len(cfg.Parameters))
	for name, spec := range cfg.Parameters {
		if spec == nil || spec.Validate == "" {
			continue
		}
		prog, err := overseer.CompileCELProgram(spec.Validate)
		if err != nil {
			return nil, fmt.Errorf("exec: action %q parameter %q: CEL compile error: %w", actionName, name, err)
		}
		if prog != nil {
			celPrograms[name] = prog
		}
	}

	if cfg.Output.Stdout.Condition != "" {
		prog, err := overseer.CompileOutputCELProgram(cfg.Output.Stdout.Condition)
		if err != nil {
			return nil, fmt.Errorf("exec: action %q stdout filter CEL error: %w", actionName, err)
		}
		cfg.Output.Stdout.prog = prog
	}

	if cfg.Output.Stderr.Condition != "" {
		prog, err := overseer.CompileOutputCELProgram(cfg.Output.Stderr.Condition)
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

// init registers the execHandlerFactory so that overseer.BuildActionHandlers
// can look it up when constructing handlers for actions of type "exec".
func init() {
	overseer.RegisterFactory(&execHandlerFactory{})
}

// ---------------------------------------------------------------------------
// renderTemplate — helper for [[ ]] delimited text/template rendering
// ---------------------------------------------------------------------------

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

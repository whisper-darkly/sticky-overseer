package overseer

import (
	"encoding/json"
	"log"
	"sort"
)

// swaggerSpec is the root Swagger 2.0 document.
type swaggerSpec struct {
	Swagger  string                     `json:"swagger"`
	Info     swaggerInfo                `json:"info"`
	BasePath string                     `json:"basePath,omitempty"`
	Consumes []string                   `json:"consumes,omitempty"`
	Produces []string                   `json:"produces,omitempty"`
	Paths    map[string]swaggerPathItem `json:"paths"`
}

type swaggerInfo struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version"`
}

// swaggerPathItem holds HTTP methods plus the custom "ws" key. sticky-bb reads
// the "ws" value; SwaggerUI ignores the unknown method.
type swaggerPathItem struct {
	Post *swaggerOperation   `json:"post,omitempty"`
	WS   *swaggerWSOperation `json:"ws,omitempty"`
}

// swaggerWSOperation describes a WebSocket connection endpoint and the commands
// that can be sent over it. Protocol-specific conventions (type discriminator
// names, correlation IDs, etc.) belong in per-command parameter defaults, not
// in this descriptor.
type swaggerWSOperation struct {
	Summary     string             `json:"summary,omitempty"`
	Description string             `json:"description,omitempty"`
	Commands    []swaggerWSCommand `json:"commands,omitempty"`
}

// swaggerWSCommand describes one message that can be sent over a WebSocket
// connection. Parameters include both visible fields (shown in the UI) and
// hidden fields (always sent with their default value, not shown to the user).
// This lets server-specific protocol conventions — like a "type" discriminator
// or an "action" name — live entirely in the spec rather than in sticky-bb.
type swaggerWSCommand struct {
	Summary     string           `json:"summary,omitempty"`
	Description string           `json:"description,omitempty"`
	OperationID string           `json:"operationId,omitempty"`
	Tags        []string         `json:"tags,omitempty"`
	ParamsKey   string           `json:"params_key,omitempty"` // nest visible params under this key
	Parameters  []swaggerWSParam `json:"parameters,omitempty"`

	// Action discovery metadata (informational).
	XActionType string       `json:"x-action-type,omitempty"`
	XTaskPool   *PoolConfig  `json:"x-task-pool,omitempty"`
	XRetry      *RetryPolicy `json:"x-retry,omitempty"`
	XDedupeKey  []string     `json:"x-dedupe-key,omitempty"`
}

// swaggerWSParam describes one field in a WebSocket command message. Hidden
// params are injected into the assembled message at the top level using their
// default value; visible params are rendered as form inputs.
// Type uses the semantic WS vocabulary: string, integer, boolean, object,
// uuid, timestamp, duration.
type swaggerWSParam struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
	Default     any    `json:"default,omitempty"`
	Validate    string `json:"validate,omitempty"`
}

// swaggerOperation describes a standard HTTP operation (used for REST paths).
type swaggerOperation struct {
	Summary     string                     `json:"summary,omitempty"`
	Description string                     `json:"description,omitempty"`
	OperationID string                     `json:"operationId,omitempty"`
	Tags        []string                   `json:"tags,omitempty"`
	Consumes    []string                   `json:"consumes,omitempty"`
	Parameters  []swaggerParameter         `json:"parameters,omitempty"`
	Responses   map[string]swaggerResponse `json:"responses"`
}

type swaggerParameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	Type        string `json:"type,omitempty"`
	Default     any    `json:"default,omitempty"`
	XValidate   string `json:"x-validate,omitempty"`
}

type swaggerResponse struct {
	Description string `json:"description"`
}

// BuildOpenAPISpec dynamically generates a Swagger 2.0 JSON document.
//
// The /ws path item uses the custom "ws" key to describe the WebSocket
// endpoint and all of its commands as a nested commands array. Protocol
// conventions such as the type discriminator and action routing are expressed
// as hidden parameters with default values — sticky-bb has no knowledge of
// overseer-specific field names.
func BuildOpenAPISpec(actions map[string]ActionHandler, version string) []byte {
	spec := swaggerSpec{
		Swagger: "2.0",
		Info: swaggerInfo{
			Title:       "sticky-overseer",
			Description: "WebSocket-based process overseer — use /ws for the full WebSocket API.",
			Version:     version,
		},
		Produces: []string{"application/json"},
		Paths:    make(map[string]swaggerPathItem),
	}

	infos := collectActions(actions)

	cmds := buildWSProtocolCommands()
	for _, info := range infos {
		cmds = append(cmds, buildWSActionCommand(info))
	}
	spec.Paths["/ws"] = swaggerPathItem{
		WS: &swaggerWSOperation{
			Summary:  "WebSocket",
			Commands: cmds,
		},
	}

	data, err := json.MarshalIndent(spec, "", "    ")
	if err != nil {
		log.Printf("openapi: failed to marshal spec: %v", err)
		return []byte(`{"swagger":"2.0","info":{"title":"sticky-overseer","version":"unknown"},"paths":{}}`)
	}
	return data
}

// hidden returns a parameter that is always injected at top-level with its
// default value and never shown in the UI.
func hidden(name string, val any) swaggerWSParam {
	return swaggerWSParam{Name: name, Default: val, Hidden: true}
}

// wsParam returns a visible parameter with the given semantic type.
func wsParam(name, description, typ string, required bool) swaggerWSParam {
	return swaggerWSParam{Name: name, Description: description, Type: typ, Required: required}
}

// buildWSProtocolCommands returns the fixed set of standard protocol commands.
// Each command carries a hidden "type" parameter that sticky-bb injects into
// every assembled message — this is sticky-overseer's protocol convention, not
// sticky-bb's.
func buildWSProtocolCommands() []swaggerWSCommand {
	id := wsParam("id", "Correlation ID (echoed in response)", "string", false)
	actionOpt := wsParam("action", "Action name (omit for all actions)", "string", false)
	taskID := wsParam("task_id", "Task identifier", "uuid", true)

	return []swaggerWSCommand{
		{
			OperationID: "ws-list", Summary: "List tasks",
			Description: "List all known tasks, optionally filtered by activity time",
			Parameters: []swaggerWSParam{
				hidden("type", "list"), id,
				wsParam("since", "Only return tasks active after this time (RFC3339)", "timestamp", false),
			},
		},
		{
			OperationID: "ws-stop", Summary: "Stop task",
			Description: "Stop a running or queued task",
			Parameters:  []swaggerWSParam{hidden("type", "stop"), id, taskID},
		},
		{
			OperationID: "ws-reset", Summary: "Reset task",
			Description: "Restart an errored task, clearing its exit history",
			Parameters:  []swaggerWSParam{hidden("type", "reset"), id, taskID},
		},
		{
			OperationID: "ws-replay", Summary: "Replay task output",
			Description: "Replay buffered output events for a task",
			Parameters: []swaggerWSParam{
				hidden("type", "replay"), id, taskID,
				wsParam("since", "Replay only output after this time (RFC3339)", "timestamp", false),
			},
		},
		{
			OperationID: "ws-describe", Summary: "Describe actions",
			Description: "Describe available actions and their parameter schemas",
			Parameters:  []swaggerWSParam{hidden("type", "describe"), id, actionOpt},
		},
		{
			OperationID: "ws-pool-info", Summary: "Pool info",
			Description: "Query current pool state for an action or globally",
			Parameters:  []swaggerWSParam{hidden("type", "pool_info"), id, actionOpt},
		},
		{
			OperationID: "ws-purge", Summary: "Purge queue",
			Description: "Remove all queued tasks for an action (or all actions)",
			Parameters:  []swaggerWSParam{hidden("type", "purge"), id, actionOpt},
		},
		{
			OperationID: "ws-set-pool", Summary: "Set pool limits",
			Description: "Dynamically update pool limits and queue configuration",
			Parameters: []swaggerWSParam{
				hidden("type", "set_pool"), id,
				wsParam("action", "Action name (omit for global)", "string", false),
				wsParam("limit", "New concurrency limit (0=unlimited)", "integer", false),
				wsParam("queue_size", "New queue size limit", "integer", false),
				wsParam("excess", "How to handle tasks exceeding the new limit", "object", false),
			},
		},
		{
			OperationID: "ws-subscribe", Summary: "Subscribe to task",
			Description: "Subscribe to task-specific broadcast events",
			Parameters:  []swaggerWSParam{hidden("type", "subscribe"), id, taskID},
		},
		{
			OperationID: "ws-unsubscribe", Summary: "Unsubscribe from task",
			Description: "Unsubscribe from task-specific broadcast events",
			Parameters:  []swaggerWSParam{hidden("type", "unsubscribe"), id, taskID},
		},
		{
			OperationID: "ws-metrics", Summary: "Get metrics",
			Description: "Query metrics at global, per-action, or per-task granularity",
			Parameters: []swaggerWSParam{
				hidden("type", "metrics"), id,
				wsParam("action", "Action name for per-action metrics (mutually exclusive with task_id)", "string", false),
				wsParam("task_id", "Task ID for per-task metrics (mutually exclusive with action)", "uuid", false),
			},
		},
		{
			OperationID: "ws-manifest", Summary: "Get manifest",
			Description: "Request the full WS Manifest document describing this protocol",
			Parameters:  []swaggerWSParam{hidden("type", "manifest"), id},
		},
	}
}

// buildWSActionCommand builds a WS command for a registered action's start
// operation. The "type" and "action" protocol fields are hidden parameters so
// sticky-bb injects them without any overseer-specific logic.
func buildWSActionCommand(info ActionInfo) swaggerWSCommand {
	summary := "Start " + info.Name + " task"
	if info.Description != "" {
		summary = info.Description
	}

	params := []swaggerWSParam{
		hidden("type", "start"),
		hidden("action", info.Name),
	}
	params = append(params, buildWSParams(info.Params)...)

	cmd := swaggerWSCommand{
		OperationID: "start-" + info.Name,
		Summary:     summary,
		Tags:        []string{info.Name},
		ParamsKey:   "params",
		Parameters:  params,
		XActionType: info.Type,
	}
	if info.TaskPool.Limit > 0 || info.TaskPool.Queue != nil {
		pool := info.TaskPool
		cmd.XTaskPool = &pool
	}
	if info.Retry != nil {
		cmd.XRetry = info.Retry
	}
	if len(info.DedupeKey) > 0 {
		cmd.XDedupeKey = info.DedupeKey
	}
	return cmd
}

// buildWSParams converts a ParamSpec map into an ordered slice of swaggerWSParam.
// Required params (Default == nil) come first.
func buildWSParams(specs map[string]*ParamSpec) []swaggerWSParam {
	if len(specs) == 0 {
		return nil
	}

	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names)

	required := make([]swaggerWSParam, 0)
	optional := make([]swaggerWSParam, 0)
	for _, name := range names {
		spec := specs[name]
		p := swaggerWSParam{Name: name, Type: "string"}
		if spec == nil || spec.Default == nil {
			p.Required = true
			if spec != nil && spec.Validate != "" {
				p.Validate = spec.Validate
			}
			required = append(required, p)
		} else {
			p.Default = *spec.Default
			if spec.Validate != "" {
				p.Validate = spec.Validate
			}
			optional = append(optional, p)
		}
	}
	return append(required, optional...)
}

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
	Tags     []swaggerTag               `json:"tags,omitempty"`
	Paths    map[string]swaggerPathItem `json:"paths"`
}

type swaggerInfo struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version"`
}

type swaggerTag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// swaggerPathItem holds the standard HTTP methods plus the custom "ws" key.
// The "ws" value is a connection-endpoint descriptor; sticky-bb consumes it
// while SwaggerUI silently ignores the unknown method.
type swaggerPathItem struct {
	Post *swaggerOperation   `json:"post,omitempty"`
	WS   *swaggerWSOperation `json:"ws,omitempty"`
}

// swaggerWSOperation describes a WebSocket connection endpoint and all of the
// commands that can be sent over it.
type swaggerWSOperation struct {
	Summary          string             `json:"summary,omitempty"`
	Description      string             `json:"description,omitempty"`
	TypeField        string             `json:"type_field,omitempty"`
	CorrelationField string             `json:"correlation_field,omitempty"`
	Commands         []swaggerWSCommand `json:"commands,omitempty"`
}

// swaggerWSCommand describes a single message that can be sent over a WebSocket
// connection. It is intentionally a separate, simpler type from swaggerOperation:
// there are no HTTP concerns (no "in", no consumes), and "type" maps directly to
// the semantic ws type rather than a restricted OAS type.
type swaggerWSCommand struct {
	Message     string           `json:"message"`
	Summary     string           `json:"summary,omitempty"`
	Description string           `json:"description,omitempty"`
	OperationID string           `json:"operationId,omitempty"`
	Tags        []string         `json:"tags,omitempty"`
	Action      string           `json:"action,omitempty"`    // inject as top-level "action" field
	ParamsKey   string           `json:"params_key,omitempty"` // wrap params under this key
	Parameters  []swaggerWSParam `json:"parameters,omitempty"`

	// Action discovery metadata (informational; used by tooling).
	XActionType string       `json:"x-action-type,omitempty"`
	XTaskPool   *PoolConfig  `json:"x-task-pool,omitempty"`
	XRetry      *RetryPolicy `json:"x-retry,omitempty"`
	XDedupeKey  []string     `json:"x-dedupe-key,omitempty"`
}

// swaggerWSParam describes a parameter for a WebSocket command. Unlike
// swaggerParameter there is no "in" field — all params are message fields.
// "type" uses the semantic WS type vocabulary directly: string, integer,
// boolean, object, uuid, timestamp, duration.
type swaggerWSParam struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required,omitempty"`
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
// The /ws path item uses the custom "ws" key (not a valid HTTP method) to
// describe the WebSocket endpoint and its commands. All WS commands — both
// standard protocol commands and registered action starts — are nested as a
// "commands" array inside that single path item. sticky-bb consumes this
// structure; SwaggerUI ignores the unknown method.
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
	actionTags := make([]swaggerTag, 0, len(infos))
	for _, info := range infos {
		tagDesc := info.Description
		if tagDesc == "" {
			tagDesc = info.Type + " action"
		}
		actionTags = append(actionTags, swaggerTag{Name: info.Name, Description: tagDesc})
	}
	sort.Slice(actionTags, func(i, j int) bool { return actionTags[i].Name < actionTags[j].Name })
	spec.Tags = append(
		[]swaggerTag{{Name: "ws-commands", Description: "Standard WebSocket protocol commands"}},
		actionTags...,
	)

	// Build the /ws path item: one connection endpoint, all commands nested inside.
	cmds := buildWSProtocolCommands()
	for _, info := range infos {
		cmds = append(cmds, buildWSActionCommand(info))
	}
	spec.Paths["/ws"] = swaggerPathItem{
		WS: &swaggerWSOperation{
			Summary:          "WebSocket",
			TypeField:        "type",
			CorrelationField: "id",
			Commands:         cmds,
		},
	}

	data, err := json.MarshalIndent(spec, "", "    ")
	if err != nil {
		log.Printf("openapi: failed to marshal spec: %v", err)
		return []byte(`{"swagger":"2.0","info":{"title":"sticky-overseer","version":"unknown"},"paths":{}}`)
	}
	return data
}

// buildWSProtocolCommands returns the fixed set of standard protocol commands.
func buildWSProtocolCommands() []swaggerWSCommand {
	p := func(name, desc, typ string, required bool) swaggerWSParam {
		return swaggerWSParam{Name: name, Description: desc, Type: typ, Required: required}
	}
	actionOpt := p("action", "Action name (omit for all actions)", "string", false)
	taskID := p("task_id", "Task identifier", "uuid", true)

	return []swaggerWSCommand{
		{
			Message: "list", OperationID: "ws-list",
			Summary:     "List tasks",
			Description: "List all known tasks, optionally filtered by activity time",
			Parameters:  []swaggerWSParam{p("since", "Only return tasks active after this time (RFC3339)", "timestamp", false)},
		},
		{
			Message: "stop", OperationID: "ws-stop",
			Summary:    "Stop task",
			Description: "Stop a running or queued task",
			Parameters: []swaggerWSParam{taskID},
		},
		{
			Message: "reset", OperationID: "ws-reset",
			Summary:    "Reset task",
			Description: "Restart an errored task, clearing its exit history",
			Parameters: []swaggerWSParam{taskID},
		},
		{
			Message: "replay", OperationID: "ws-replay",
			Summary:    "Replay task output",
			Description: "Replay buffered output events for a task",
			Parameters: []swaggerWSParam{
				taskID,
				p("since", "Replay only output after this time (RFC3339)", "timestamp", false),
			},
		},
		{
			Message: "describe", OperationID: "ws-describe",
			Summary:    "Describe actions",
			Description: "Describe available actions and their parameter schemas",
			Parameters: []swaggerWSParam{actionOpt},
		},
		{
			Message: "pool_info", OperationID: "ws-pool-info",
			Summary:    "Pool info",
			Description: "Query current pool state for an action or globally",
			Parameters: []swaggerWSParam{actionOpt},
		},
		{
			Message: "purge", OperationID: "ws-purge",
			Summary:    "Purge queue",
			Description: "Remove all queued tasks for an action (or all actions)",
			Parameters: []swaggerWSParam{actionOpt},
		},
		{
			Message: "set_pool", OperationID: "ws-set-pool",
			Summary:    "Set pool limits",
			Description: "Dynamically update pool limits and queue configuration",
			Parameters: []swaggerWSParam{
				p("action", "Action name (omit for global)", "string", false),
				p("limit", "New concurrency limit (0=unlimited)", "integer", false),
				p("queue_size", "New queue size limit", "integer", false),
				p("excess", "How to handle tasks exceeding the new limit", "object", false),
			},
		},
		{
			Message: "subscribe", OperationID: "ws-subscribe",
			Summary:    "Subscribe to task",
			Description: "Subscribe to task-specific broadcast events",
			Parameters: []swaggerWSParam{taskID},
		},
		{
			Message: "unsubscribe", OperationID: "ws-unsubscribe",
			Summary:    "Unsubscribe from task",
			Description: "Unsubscribe from task-specific broadcast events",
			Parameters: []swaggerWSParam{taskID},
		},
		{
			Message: "metrics", OperationID: "ws-metrics",
			Summary:    "Get metrics",
			Description: "Query metrics at global, per-action, or per-task granularity",
			Parameters: []swaggerWSParam{
				p("action", "Action name for per-action metrics (mutually exclusive with task_id)", "string", false),
				p("task_id", "Task ID for per-task metrics (mutually exclusive with action)", "uuid", false),
			},
		},
		{
			Message: "manifest", OperationID: "ws-manifest",
			Summary:    "Get manifest",
			Description: "Request the full WS Manifest document describing this protocol",
		},
	}
}

// buildWSActionCommand builds a WS command descriptor for a registered action's
// start operation.
func buildWSActionCommand(info ActionInfo) swaggerWSCommand {
	summary := "Start " + info.Name + " task"
	if info.Description != "" {
		summary = info.Description
	}

	cmd := swaggerWSCommand{
		Message:     "start",
		OperationID: "start-" + info.Name,
		Summary:     summary,
		Tags:        []string{info.Name},
		Action:      info.Name,
		ParamsKey:   "params",
		Parameters:  buildWSParams(info.Params),
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

package overseer

import (
	"encoding/json"
	"log"
	"sort"
)

// swaggerSpec is the root Swagger 2.0 document.
type swaggerSpec struct {
	Swagger             string                        `json:"swagger"`
	Info                swaggerInfo                   `json:"info"`
	BasePath            string                        `json:"basePath,omitempty"`
	Consumes            []string                      `json:"consumes,omitempty"`
	Produces            []string                      `json:"produces,omitempty"`
	Tags                []swaggerTag                  `json:"tags,omitempty"`
	Paths               map[string]swaggerPathItem    `json:"paths"`
	XWebsocketEndpoints map[string]swaggerWSEndpoint  `json:"x-websocket-endpoints,omitempty"`
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

// swaggerWSEndpoint describes a WebSocket connection endpoint for x-websocket-endpoints.
type swaggerWSEndpoint struct {
	Label            string `json:"label,omitempty"`
	TypeField        string `json:"type_field"`
	CorrelationField string `json:"correlation_field,omitempty"`
}

type swaggerPathItem struct {
	Post *swaggerOperation `json:"post,omitempty"`
	WS   *swaggerOperation `json:"ws,omitempty"`
}

type swaggerOperation struct {
	Summary     string                     `json:"summary,omitempty"`
	Description string                     `json:"description,omitempty"`
	OperationID string                     `json:"operationId,omitempty"`
	Tags        []string                   `json:"tags,omitempty"`
	Consumes    []string                   `json:"consumes,omitempty"`
	Parameters  []swaggerParameter         `json:"parameters,omitempty"`
	Responses   map[string]swaggerResponse `json:"responses"`

	// Custom extensions with action metadata.
	XActionType string       `json:"x-action-type,omitempty"`
	XTaskPool   *PoolConfig  `json:"x-task-pool,omitempty"`
	XRetry      *RetryPolicy `json:"x-retry,omitempty"`
	XDedupeKey  []string     `json:"x-dedupe-key,omitempty"`

	// WebSocket extensions.
	XWSEndpoint  string `json:"x-ws-endpoint,omitempty"`
	XWSMessage   string `json:"x-ws-message,omitempty"`
	XWSAction    string `json:"x-ws-action,omitempty"`
	XWSParamsKey string `json:"x-ws-params-key,omitempty"`
}

type swaggerParameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	Type        string `json:"type,omitempty"`
	Default     any    `json:"default,omitempty"`
	XValidate   string `json:"x-validate,omitempty"`
	XWSType     string `json:"x-ws-type,omitempty"`
}

type swaggerResponse struct {
	Description string `json:"description"`
}

// BuildOpenAPISpec dynamically generates a Swagger 2.0 JSON document. WS commands and
// action start operations are exposed as "ws" method path items (an intentionally invalid
// HTTP method that sticky-bb intercepts for its WS console). Standard HTTP paths are not
// generated for WS operations. Returns the JSON-encoded spec; on error it logs and returns
// a minimal valid spec.
func BuildOpenAPISpec(actions map[string]ActionHandler, version string) []byte {
	spec := swaggerSpec{
		Swagger: "2.0",
		Info: swaggerInfo{
			Title:       "sticky-overseer",
			Description: "WebSocket-based process overseer — use /ws for the full WebSocket API.",
			Version:     version,
		},
		Produces: []string{"application/json"},
		XWebsocketEndpoints: map[string]swaggerWSEndpoint{
			"/ws": {Label: "WebSocket", TypeField: "type", CorrelationField: "id"},
		},
		Paths: make(map[string]swaggerPathItem),
	}

	// Collect and sort action names for deterministic output.
	infos := collectActions(actions)
	actionTags := make([]swaggerTag, 0, len(infos))

	for _, info := range infos {
		// Build tag per action.
		tagDesc := info.Description
		if tagDesc == "" {
			tagDesc = info.Type + " action"
		}
		actionTags = append(actionTags, swaggerTag{
			Name:        info.Name,
			Description: tagDesc,
		})

		// Build form parameters from ParamSpec.
		params := buildSwaggerParams(info.Params)

		// Build WS operation for this action at /ws/actions/{name}.
		summary := "Start " + info.Name + " task"
		if info.Description != "" {
			summary = info.Description
		}

		wsOp := buildWSCommandOperation(summary, "", "start-"+info.Name, "start", params)
		wsOp.Tags = []string{info.Name}
		wsOp.XWSAction = info.Name
		wsOp.XWSParamsKey = "params"
		wsOp.XActionType = info.Type

		// Include optional metadata as extensions only when populated.
		if info.TaskPool.Limit > 0 || info.TaskPool.Queue != nil {
			pool := info.TaskPool
			wsOp.XTaskPool = &pool
		}
		if info.Retry != nil {
			wsOp.XRetry = info.Retry
		}
		if len(info.DedupeKey) > 0 {
			wsOp.XDedupeKey = info.DedupeKey
		}

		path := "/ws/actions/" + info.Name
		spec.Paths[path] = swaggerPathItem{WS: wsOp}
	}

	// ws-commands tag first, then action tags sorted alphabetically.
	sort.Slice(actionTags, func(i, j int) bool { return actionTags[i].Name < actionTags[j].Name })
	spec.Tags = append(
		[]swaggerTag{{Name: "ws-commands", Description: "Standard WebSocket protocol commands"}},
		actionTags...,
	)

	// Add standard WS protocol command paths.
	addWSCommandPaths(spec.Paths)

	data, err := json.MarshalIndent(spec, "", "    ")
	if err != nil {
		log.Printf("openapi: failed to marshal spec: %v", err)
		return []byte(`{"swagger":"2.0","info":{"title":"sticky-overseer","version":"unknown"},"paths":{}}`)
	}
	return data
}

// addWSCommandPaths populates the standard protocol command paths with ws-method operations.
func addWSCommandPaths(paths map[string]swaggerPathItem) {
	add := func(path, message, summary, description, opID string, params []swaggerParameter) {
		paths[path] = swaggerPathItem{
			WS: buildWSCommandOperation(summary, description, opID, message, params),
		}
	}

	add("/ws/list", "list", "List tasks",
		"List all known tasks, optionally filtered by activity time", "ws-list",
		[]swaggerParameter{
			wsParam("since", "Only return tasks active after this time (RFC3339)", "timestamp", false),
		})

	add("/ws/stop", "stop", "Stop task",
		"Stop a running or queued task", "ws-stop",
		[]swaggerParameter{
			wsParam("task_id", "Task identifier", "uuid", true),
		})

	add("/ws/reset", "reset", "Reset task",
		"Restart an errored task, clearing its exit history", "ws-reset",
		[]swaggerParameter{
			wsParam("task_id", "Task identifier", "uuid", true),
		})

	add("/ws/replay", "replay", "Replay task output",
		"Replay buffered output events for a task directly to the requesting connection", "ws-replay",
		[]swaggerParameter{
			wsParam("task_id", "Task identifier", "uuid", true),
			wsParam("since", "Replay only output after this time (RFC3339)", "timestamp", false),
		})

	add("/ws/describe", "describe", "Describe actions",
		"Describe available actions and their parameter schemas", "ws-describe",
		[]swaggerParameter{
			wsParam("action", "Action name (omit for all actions)", "string", false),
		})

	add("/ws/pool_info", "pool_info", "Pool info",
		"Query current pool state for an action or globally", "ws-pool-info",
		[]swaggerParameter{
			wsParam("action", "Action name (omit for all actions)", "string", false),
		})

	add("/ws/purge", "purge", "Purge queue",
		"Remove all queued tasks for an action (or all actions)", "ws-purge",
		[]swaggerParameter{
			wsParam("action", "Action name (omit for all actions)", "string", false),
		})

	add("/ws/set_pool", "set_pool", "Set pool limits",
		"Dynamically update pool limits and queue configuration", "ws-set-pool",
		[]swaggerParameter{
			wsParam("action", "Action name (omit for global)", "string", false),
			wsParam("limit", "New concurrency limit (0=unlimited)", "integer", false),
			wsParam("queue_size", "New queue size limit", "integer", false),
			wsParam("excess", "How to handle tasks exceeding the new limit", "object", false),
		})

	add("/ws/subscribe", "subscribe", "Subscribe to task",
		"Subscribe to task-specific broadcast events", "ws-subscribe",
		[]swaggerParameter{
			wsParam("task_id", "Task identifier", "uuid", true),
		})

	add("/ws/unsubscribe", "unsubscribe", "Unsubscribe from task",
		"Unsubscribe from task-specific broadcast events", "ws-unsubscribe",
		[]swaggerParameter{
			wsParam("task_id", "Task identifier", "uuid", true),
		})

	add("/ws/metrics", "metrics", "Get metrics",
		"Query metrics at global, per-action, or per-task granularity", "ws-metrics",
		[]swaggerParameter{
			wsParam("action", "Action name for per-action metrics (mutually exclusive with task_id)", "string", false),
			wsParam("task_id", "Task ID for per-task metrics (mutually exclusive with action)", "uuid", false),
		})

	add("/ws/manifest", "manifest", "Get manifest",
		"Request the full WS Manifest document describing this protocol", "ws-manifest", nil)
}

// wsParam builds a formData swaggerParameter with the given semantic ws type hint.
func wsParam(name, description, wsType string, required bool) swaggerParameter {
	p := swaggerParameter{
		Name:        name,
		In:          "formData",
		Description: description,
		Required:    required,
		XWSType:     wsType,
	}
	// Map ws type to a valid Swagger formData type.
	switch wsType {
	case "integer":
		p.Type = "integer"
	default:
		p.Type = "string"
	}
	return p
}

// buildWSCommandOperation creates a WS-method operation with common ws-commands tag and
// x-ws-endpoint/x-ws-message extensions pre-filled.
func buildWSCommandOperation(summary, description, opID, wsMessage string, params []swaggerParameter) *swaggerOperation {
	return &swaggerOperation{
		Summary:     summary,
		Description: description,
		OperationID: opID,
		Tags:        []string{"ws-commands"},
		Consumes:    []string{"application/x-www-form-urlencoded"},
		Parameters:  params,
		Responses: map[string]swaggerResponse{
			"200": {Description: "Message sent"},
		},
		XWSEndpoint: "/ws",
		XWSMessage:  wsMessage,
	}
}

// buildSwaggerParams converts a ParamSpec map into an ordered slice of
// Swagger 2.0 formData parameters. Required params (Default == nil) come first.
func buildSwaggerParams(specs map[string]*ParamSpec) []swaggerParameter {
	if len(specs) == 0 {
		return nil
	}

	// Collect and sort param names for deterministic ordering.
	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names)

	// Required params before optional.
	required := make([]swaggerParameter, 0)
	optional := make([]swaggerParameter, 0)
	for _, name := range names {
		spec := specs[name]
		p := swaggerParameter{
			Name: name,
			In:   "formData",
			Type: "string",
		}
		if spec == nil || spec.Default == nil {
			p.Required = true
			required = append(required, p)
		} else {
			p.Required = false
			p.Default = *spec.Default
			if spec.Validate != "" {
				p.XValidate = spec.Validate
			}
			optional = append(optional, p)
		}
		if spec != nil && spec.Validate != "" && spec.Default == nil {
			// Required param with validation.
			required[len(required)-1].XValidate = spec.Validate
		}
	}

	return append(required, optional...)
}

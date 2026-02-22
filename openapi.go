package overseer

import (
	"encoding/json"
	"log"
	"sort"
)

// swaggerSpec is the root Swagger 2.0 document.
type swaggerSpec struct {
	Swagger  string                      `json:"swagger"`
	Info     swaggerInfo                 `json:"info"`
	BasePath string                      `json:"basePath,omitempty"`
	Consumes []string                    `json:"consumes,omitempty"`
	Produces []string                    `json:"produces,omitempty"`
	Tags     []swaggerTag                `json:"tags,omitempty"`
	Paths    map[string]swaggerPathItem  `json:"paths"`
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

type swaggerPathItem struct {
	Post *swaggerOperation `json:"post,omitempty"`
}

type swaggerOperation struct {
	Summary     string                      `json:"summary,omitempty"`
	Description string                      `json:"description,omitempty"`
	OperationID string                      `json:"operationId,omitempty"`
	Tags        []string                    `json:"tags,omitempty"`
	Consumes    []string                    `json:"consumes,omitempty"`
	Parameters  []swaggerParameter          `json:"parameters,omitempty"`
	Responses   map[string]swaggerResponse  `json:"responses"`

	// Custom extensions with action metadata.
	XActionType string      `json:"x-action-type,omitempty"`
	XTaskPool   *PoolConfig `json:"x-task-pool,omitempty"`
	XRetry      *RetryPolicy `json:"x-retry,omitempty"`
	XDedupeKey  []string    `json:"x-dedupe-key,omitempty"`
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

// BuildOpenAPISpec dynamically generates a Swagger 2.0 JSON document from the
// registered action handlers. Each action is exposed as a POST path under
// /actions/{actionName}/start with formData parameters derived from the
// action's ParamSpec metadata. Returns the JSON-encoded spec; on error it
// logs and returns a minimal valid spec.
func BuildOpenAPISpec(actions map[string]ActionHandler, version string) []byte {
	spec := swaggerSpec{
		Swagger: "2.0",
		Info: swaggerInfo{
			Title:       "sticky-overseer",
			Description: "WebSocket-based process overseer — REST action discovery endpoint. Use /ws for the full WebSocket API.",
			Version:     version,
		},
		Produces: []string{"application/json"},
		Paths:    make(map[string]swaggerPathItem),
	}

	// Collect and sort action names for deterministic output.
	infos := collectActions(actions)
	tags := make([]swaggerTag, 0, len(infos))

	for _, info := range infos {
		// Build tag per action.
		tagDesc := info.Description
		if tagDesc == "" {
			tagDesc = info.Type + " action"
		}
		tags = append(tags, swaggerTag{
			Name:        info.Name,
			Description: tagDesc,
		})

		// Build form parameters from ParamSpec.
		params := buildSwaggerParams(info.Params)

		// Build the operation.
		summary := "Start " + info.Name + " task"
		if info.Description != "" {
			summary = info.Description
		}

		op := &swaggerOperation{
			Summary:     summary,
			OperationID: "start-" + info.Name,
			Tags:        []string{info.Name},
			Consumes:    []string{"application/x-www-form-urlencoded"},
			Parameters:  params,
			Responses: map[string]swaggerResponse{
				"200": {Description: "Task started or queued"},
				"400": {Description: "Invalid parameters"},
				"409": {Description: "Task already active or duplicate"},
			},
			XActionType: info.Type,
		}

		// Include optional metadata as extensions only when populated.
		if info.TaskPool.Limit > 0 || info.TaskPool.Queue != nil {
			pool := info.TaskPool
			op.XTaskPool = &pool
		}
		if info.Retry != nil {
			op.XRetry = info.Retry
		}
		if len(info.DedupeKey) > 0 {
			op.XDedupeKey = info.DedupeKey
		}

		path := "/actions/" + info.Name + "/start"
		spec.Paths[path] = swaggerPathItem{Post: op}
	}

	// Sort tags by name (collectActions already sorts infos, so this mirrors it).
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
	spec.Tags = tags

	data, err := json.MarshalIndent(spec, "", "    ")
	if err != nil {
		log.Printf("openapi: failed to marshal spec: %v", err)
		return []byte(`{"swagger":"2.0","info":{"title":"sticky-overseer","version":"unknown"},"paths":{}}`)
	}
	return data
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

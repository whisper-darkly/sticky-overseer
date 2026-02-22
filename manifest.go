package overseer

import "sort"

// WSManifest is the document returned by GET /ws/manifest and the "manifest" WS message.
type WSManifest struct {
	Schema      string                     `json:"$schema,omitempty"`
	Protocol    string                     `json:"protocol"`
	Version     string                     `json:"version"`
	Description string                     `json:"description"`
	Transport   ManifestTransport          `json:"transport"`
	Types       map[string]ManifestType    `json:"types"`
	Messages    map[string]ManifestMessage `json:"messages"`
	Operations  map[string]ManifestOp      `json:"operations"`
	Actions     []ActionInfo               `json:"actions"`
}

// ManifestTransport describes how to connect to the WebSocket endpoint.
type ManifestTransport struct {
	Path             string `json:"path"`
	TypeField        string `json:"type_field"`
	CorrelationField string `json:"correlation_field,omitempty"`
}

// ManifestType describes a named type in the manifest type system.
type ManifestType struct {
	Type        string                   `json:"type,omitempty"`
	Description string                   `json:"description,omitempty"`
	Values      []string                 `json:"values,omitempty"`
	Fields      map[string]ManifestField `json:"fields,omitempty"`
	Items       any                      `json:"items,omitempty"`
	ValueType   any                      `json:"value_type,omitempty"`
}

// ManifestField describes a single field within a message or type.
type ManifestField struct {
	Type        string                   `json:"type,omitempty"`
	Ref         string                   `json:"$ref,omitempty"`
	Description string                   `json:"description,omitempty"`
	Required    bool                     `json:"required,omitempty"`
	Default     any                      `json:"default,omitempty"`
	UIHint      string                   `json:"ui_hint,omitempty"`
	Values      []string                 `json:"values,omitempty"`
	Fields      map[string]ManifestField `json:"fields,omitempty"`
	Items       any                      `json:"items,omitempty"`
	ValueType   any                      `json:"value_type,omitempty"`
}

// ManifestMessage describes a single WebSocket message type.
type ManifestMessage struct {
	Description string                   `json:"description"`
	Direction   string                   `json:"direction"`
	Delivery    string                   `json:"delivery,omitempty"`
	Fields      map[string]ManifestField `json:"fields"`
}

// ManifestOp describes a logical request/response operation.
type ManifestOp struct {
	Description string              `json:"description"`
	Send        string              `json:"send"`
	Responses   ManifestOpResponses `json:"responses"`
	Notes       string              `json:"notes,omitempty"`
}

// ManifestOpResponses groups direct and side-effect responses for an operation.
type ManifestOpResponses struct {
	Direct      *ManifestOpDirect      `json:"direct,omitempty"`
	SideEffects *ManifestOpSideEffects `json:"side_effects,omitempty"`
}

// ManifestOpDirect describes the direct success/error message types for an operation.
type ManifestOpDirect struct {
	Success []string `json:"success,omitempty"`
	Error   []string `json:"error,omitempty"`
}

// ManifestOpSideEffects describes broadcast side-effects triggered by an operation.
type ManifestOpSideEffects struct {
	Broadcast            []string `json:"broadcast,omitempty"`
	BroadcastSubscribers []string `json:"broadcast_subscribers,omitempty"`
}

// BuildManifest constructs a WSManifest for the current server instance.
// actions is the runtime handler map (may be nil — produces empty actions list).
// version is the server's version string.
func BuildManifest(actions map[string]ActionHandler, version string) WSManifest {
	m := WSManifest{
		Schema:      "https://github.com/whisper-darkly/sticky-bb/raw/main/docs/ws-manifest.schema.json",
		Protocol:    "sticky-overseer",
		Version:     version,
		Description: "WebSocket-based process overseer for spawning, tracking, and streaming output from child processes",
		Transport: ManifestTransport{
			Path:             "/ws",
			TypeField:        "type",
			CorrelationField: "id",
		},
		Types:      buildManifestTypes(),
		Messages:   buildManifestMessages(),
		Operations: buildManifestOperations(),
		Actions:    collectActions(actions),
	}
	return m
}

func buildManifestTypes() map[string]ManifestType {
	ref := func(name string) ManifestField {
		return ManifestField{Ref: "#/types/" + name}
	}
	_ = ref // suppress unused if needed

	return map[string]ManifestType{
		"TaskState": {
			Type:        "enum",
			Description: "Lifecycle state of a task",
			Values:      []string{"active", "stopped", "errored", "queued"},
		},
		"WorkerState": {
			Type:        "enum",
			Description: "Running state of a worker process",
			Values:      []string{"running", "exited"},
		},
		"Stream": {
			Type:        "enum",
			Description: "Output stream identifier",
			Values:      []string{"stdout", "stderr"},
		},
		"QueueOrder": {
			Type:        "enum",
			Description: "Dequeue order: first=FIFO, last=LIFO",
			Values:      []string{"first", "last"},
		},
		"ExcessAction": {
			Type:        "enum",
			Description: "What to do when pool limit decreases while tasks are running",
			Values:      []string{"stop", "requeue"},
		},
		"KillOrder": {
			Type:        "enum",
			Description: "Which running task to affect first: first=longest-running, last=shortest-running",
			Values:      []string{"first", "last"},
		},
		"RetryPolicy": {
			Type:        "object",
			Description: "Controls automatic restarts after non-intentional worker exits",
			Fields: map[string]ManifestField{
				"restart_delay":   {Type: "duration", Description: "Delay between exit and restart attempt"},
				"error_window":    {Type: "duration", Description: "Time window for counting exits toward error threshold"},
				"error_threshold": {Type: "integer", Description: "Max exits within error_window before task is errored"},
			},
		},
		"DisplacePolicy": {
			Type:        "object",
			Description: "Controls whether queued tasks displace running ones when the queue is full",
			Fields: map[string]ManifestField{
				"enabled": {Type: "boolean", Description: "Whether displacement is active"},
				"order":   {Ref: "#/types/KillOrder", Description: "Which running task to displace"},
			},
		},
		"QueueConfig": {
			Type:        "object",
			Description: "Controls queuing behavior when the pool limit is reached",
			Fields: map[string]ManifestField{
				"enabled":  {Type: "boolean", Required: true, Description: "Whether queuing is enabled"},
				"size":     {Type: "integer", Description: "Max queue depth (0=unlimited)"},
				"order":    {Ref: "#/types/QueueOrder", Description: "Dequeue order"},
				"max_age":  {Type: "integer", Description: "Max seconds a task waits in queue before expiry (0=no expiry)"},
				"displace": {Ref: "#/types/DisplacePolicy", Description: "Whether to displace running tasks when queue is full"},
			},
		},
		"ExcessConfig": {
			Type:        "object",
			Description: "Defines behavior when running pool limit is reduced at runtime via set_pool",
			Fields: map[string]ManifestField{
				"action": {Ref: "#/types/ExcessAction", Description: "What to do with excess running tasks"},
				"order":  {Ref: "#/types/KillOrder", Description: "Which tasks to affect first"},
				"grace":  {Type: "integer", Description: "Seconds to wait for graceful stop before SIGKILL"},
			},
		},
		"PoolConfig": {
			Type:        "object",
			Description: "Concurrency configuration for an action (global or per-action)",
			Fields: map[string]ManifestField{
				"limit":  {Type: "integer", Required: true, Description: "Max concurrent running tasks (0=unlimited)"},
				"queue":  {Ref: "#/types/QueueConfig", Description: "Queue configuration when limit is reached"},
				"excess": {Ref: "#/types/ExcessConfig", Description: "Excess handling when limit decreases"},
			},
		},
		"PoolInfo": {
			Type:        "object",
			Description: "Point-in-time snapshot of pool state",
			Fields: map[string]ManifestField{
				"limit":       {Type: "integer", Required: true},
				"running":     {Type: "integer", Required: true},
				"queue_depth": {Type: "integer", Required: true},
				"queue_items": {
					Type: "array",
					Items: map[string]any{"type": "object", "fields": map[string]any{
						"task_id": map[string]any{"type": "string"},
						"action":  map[string]any{"type": "string"},
					}},
				},
			},
		},
		"ParamSpec": {
			Type:        "object",
			Description: "Describes a single action parameter",
			Fields: map[string]ManifestField{
				"default":  {Type: "string", Description: "Default value; null means the parameter is required"},
				"validate": {Type: "string", Description: "CEL expression operating on 'value' (string); empty means no validation"},
			},
		},
		"ActionInfo": {
			Type:        "object",
			Description: "Describes a registered action and its parameters",
			Fields: map[string]ManifestField{
				"name":        {Type: "string", Required: true},
				"type":        {Type: "string", Required: true, Description: "Driver/factory type (e.g. exec)"},
				"description": {Type: "string"},
				"params":      {Type: "map", ValueType: ManifestField{Ref: "#/types/ParamSpec"}, Description: "Accepted parameters"},
				"task_pool":   {Ref: "#/types/PoolConfig"},
				"retry":       {Ref: "#/types/RetryPolicy"},
				"dedupe_key":  {Type: "array", Items: ManifestField{Type: "string"}, Description: "Param names forming the deduplication identity"},
			},
		},
		"TaskInfo": {
			Type:        "object",
			Description: "Full state record for a task",
			Fields: map[string]ManifestField{
				"task_id":         {Type: "uuid", Required: true},
				"action":          {Type: "string", Required: true},
				"params":          {Type: "map", ValueType: ManifestField{Type: "string"}},
				"queued_at":       {Type: "timestamp"},
				"state":           {Ref: "#/types/TaskState", Required: true},
				"retry_policy":    {Ref: "#/types/RetryPolicy"},
				"current_pid":     {Type: "integer"},
				"worker_state":    {Ref: "#/types/WorkerState"},
				"restart_count":   {Type: "integer", Required: true},
				"created_at":      {Type: "timestamp", Required: true},
				"last_started_at": {Type: "timestamp"},
				"last_exited_at":  {Type: "timestamp"},
				"last_exit_code":  {Type: "integer"},
				"error_message":   {Type: "string"},
			},
		},
		"TaskMetrics": {
			Type:        "object",
			Description: "Lifetime stats for a single task",
			Fields: map[string]ManifestField{
				"output_lines":   {Type: "integer"},
				"restart_count":  {Type: "integer"},
				"runtime_ms":     {Type: "integer"},
				"last_exit_code": {Type: "integer"},
			},
		},
		"ActionMetrics": {
			Type:        "object",
			Description: "Aggregate stats for all tasks of one action type",
			Fields: map[string]ManifestField{
				"tasks_started":      {Type: "integer"},
				"tasks_completed":    {Type: "integer"},
				"tasks_errored":      {Type: "integer"},
				"tasks_restarted":    {Type: "integer"},
				"total_output_lines": {Type: "integer"},
			},
		},
		"GlobalMetrics": {
			Type:        "object",
			Description: "Global aggregate counters since server start",
			Fields: map[string]ManifestField{
				"tasks_started":      {Type: "integer"},
				"tasks_completed":    {Type: "integer"},
				"tasks_errored":      {Type: "integer"},
				"tasks_restarted":    {Type: "integer"},
				"total_output_lines": {Type: "integer"},
				"enqueued":           {Type: "integer"},
				"dequeued":           {Type: "integer"},
				"displaced":          {Type: "integer"},
				"expired":            {Type: "integer"},
			},
		},
	}
}

func buildManifestMessages() map[string]ManifestMessage {
	// Shared field definitions
	typeField := func(typeName string) ManifestField {
		return ManifestField{Type: "string", Required: true, UIHint: "hidden", Default: typeName}
	}
	idField := ManifestField{Type: "string", Description: "Client correlation ID; echoed in direct response"}
	idEchoField := ManifestField{Type: "string", Description: "Echoes the client's correlation ID"}
	taskIDField := ManifestField{Type: "uuid", Required: true, Description: "Task identifier"}
	taskIDOptField := ManifestField{Type: "uuid", Description: "Task identifier (server-generated if omitted)"}
	tsField := ManifestField{Type: "timestamp", Required: true, Description: "Event timestamp (UTC)"}
	actionOptField := ManifestField{Type: "string", Description: "Action name (omit for all actions)"}

	msgs := map[string]ManifestMessage{
		// --- c2s messages ---
		"start": {
			Description: "Enqueue or start a new task for the named action",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":         typeField("start"),
				"id":           idField,
				"action":       {Type: "string", Required: true, Description: "Action name to execute"},
				"task_id":      taskIDOptField,
				"params":       {Type: "map", ValueType: ManifestField{Type: "string"}, Description: "Action-specific parameters"},
				"force":        {Type: "boolean", Default: false, Description: "Force start even when pool is at capacity (bypasses queue)"},
				"retry_policy": {Ref: "#/types/RetryPolicy", Description: "Override the action's default retry policy for this task"},
			},
		},
		"stop": {
			Description: "Stop a running or queued task",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":    typeField("stop"),
				"id":      idField,
				"task_id": taskIDField,
			},
		},
		"reset": {
			Description: "Restart an errored task, clearing its exit history",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":    typeField("reset"),
				"id":      idField,
				"task_id": taskIDField,
			},
		},
		"list": {
			Description: "List all known tasks, optionally filtered by activity time",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":  typeField("list"),
				"id":    idField,
				"since": {Type: "timestamp", Description: "Only return tasks that were active after this time (RFC3339)"},
			},
		},
		"replay": {
			Description: "Replay buffered output events for a task directly to the requesting connection",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":    typeField("replay"),
				"id":      idField,
				"task_id": taskIDField,
				"since":   {Type: "timestamp", Description: "Replay only output after this time (RFC3339)"},
			},
		},
		"describe": {
			Description: "Describe available actions and their parameter schemas",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":   typeField("describe"),
				"id":     idField,
				"action": actionOptField,
			},
		},
		"pool_info": {
			Description: "Query current pool state for an action or globally",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":   typeField("pool_info"),
				"id":     idField,
				"action": actionOptField,
			},
		},
		"purge": {
			Description: "Remove all queued tasks for an action (or all actions)",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":   typeField("purge"),
				"id":     idField,
				"action": actionOptField,
			},
		},
		"set_pool": {
			Description: "Dynamically update pool limits and queue configuration",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":       typeField("set_pool"),
				"id":         idField,
				"action":     actionOptField,
				"limit":      {Type: "integer", Description: "New concurrency limit (0=unlimited)"},
				"queue_size": {Type: "integer", Description: "New queue size limit"},
				"excess":     {Ref: "#/types/ExcessConfig", Description: "How to handle tasks that exceed the new limit"},
			},
		},
		"subscribe": {
			Description: "Subscribe to task-specific broadcast events (output, exited, restarting, errored)",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":    typeField("subscribe"),
				"id":      idField,
				"task_id": taskIDField,
			},
		},
		"unsubscribe": {
			Description: "Unsubscribe from task-specific broadcast events",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":    typeField("unsubscribe"),
				"id":      idField,
				"task_id": taskIDField,
			},
		},
		"metrics": {
			Description: "Query metrics at global, per-action, or per-task granularity",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type":    typeField("metrics"),
				"id":      idField,
				"action":  {Type: "string", Description: "Action name for per-action metrics (mutually exclusive with task_id)"},
				"task_id": {Type: "uuid", Description: "Task ID for per-task metrics (mutually exclusive with action)"},
			},
		},
		"manifest": {
			Description: "Request the full WS Manifest document describing this protocol",
			Direction:   "c2s",
			Fields: map[string]ManifestField{
				"type": typeField("manifest"),
				"id":   idField,
			},
		},

		// --- s2c messages ---
		"started": {
			Description: "Broadcast to task subscribers when a task's worker process has started (or restarted)",
			Direction:   "s2c",
			Delivery:    "broadcast_subscribers",
			Fields: map[string]ManifestField{
				"type":       {Type: "string", UIHint: "hidden"},
				"task_id":    {Type: "uuid", Required: true},
				"pid":        {Type: "integer", Required: true, Description: "OS process ID of the worker"},
				"restart_of": {Type: "integer", Description: "PID of the previous worker if this is a restart; 0 otherwise"},
				"ts":         tsField,
			},
		},
		"tasks": {
			Description: "Direct response to a list request containing all matching task records",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":  {Type: "string", UIHint: "hidden"},
				"id":    idEchoField,
				"tasks": {Type: "array", Required: true, Items: ManifestField{Ref: "#/types/TaskInfo"}},
			},
		},
		"output": {
			Description: "Broadcast to task subscribers for each line of stdout/stderr output from the worker",
			Direction:   "s2c",
			Delivery:    "broadcast_subscribers",
			Fields: map[string]ManifestField{
				"type":    {Type: "string", UIHint: "hidden"},
				"task_id": {Type: "uuid", Required: true},
				"pid":     {Type: "integer", Required: true},
				"stream":  {Ref: "#/types/Stream", Required: true},
				"data":    {Type: "string", Required: true, Description: "Output line content"},
				"ts":      tsField,
				"seq":     {Type: "integer", Description: "Per-task monotonic sequence number for ordering output lines"},
			},
		},
		"exited": {
			Description: "Broadcast to task subscribers when the worker process exits",
			Direction:   "s2c",
			Delivery:    "broadcast_subscribers",
			Fields: map[string]ManifestField{
				"type":        {Type: "string", UIHint: "hidden"},
				"task_id":     {Type: "uuid", Required: true},
				"pid":         {Type: "integer", Required: true},
				"exit_code":   {Type: "integer", Required: true},
				"intentional": {Type: "boolean", Required: true, Description: "True if the exit was caused by a stop command"},
				"ts":          tsField,
			},
		},
		"restarting": {
			Description: "Broadcast to task subscribers when an automatic restart is scheduled",
			Direction:   "s2c",
			Delivery:    "broadcast_subscribers",
			Fields: map[string]ManifestField{
				"type":          {Type: "string", UIHint: "hidden"},
				"task_id":       {Type: "uuid", Required: true},
				"pid":           {Type: "integer", Required: true},
				"restart_delay": {Type: "duration", Required: true, Description: "How long until the restart attempt"},
				"attempt":       {Type: "integer", Required: true, Description: "Restart attempt number (1-based)"},
				"ts":            tsField,
			},
		},
		"errored": {
			Description: "Broadcast to task subscribers when a task exceeds its error threshold and is permanently stopped",
			Direction:   "s2c",
			Delivery:    "broadcast_subscribers",
			Fields: map[string]ManifestField{
				"type":       {Type: "string", UIHint: "hidden"},
				"task_id":    {Type: "uuid", Required: true},
				"pid":        {Type: "integer", Required: true},
				"exit_count": {Type: "integer", Required: true, Description: "Number of non-intentional exits that triggered the error state"},
				"ts":         tsField,
			},
		},
		"error": {
			Description: "Direct error response to a client request, or protocol-level error notification",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":             {Type: "string", UIHint: "hidden"},
				"id":               idEchoField,
				"message":          {Type: "string", Required: true, Description: "Human-readable error description"},
				"existing_task_id": {Type: "uuid", Description: "Set when a start request was rejected due to a duplicate task"},
			},
		},
		"queued": {
			Description: "Broadcast to all clients when a task is accepted but placed in the queue (pool at capacity)",
			Direction:   "s2c",
			Delivery:    "broadcast",
			Fields: map[string]ManifestField{
				"type":     {Type: "string", UIHint: "hidden"},
				"id":       idEchoField,
				"task_id":  {Type: "uuid", Required: true},
				"action":   {Type: "string", Required: true},
				"position": {Type: "integer", Required: true, Description: "Queue position (1-based)"},
				"ts":       tsField,
			},
		},
		"dequeued": {
			Description: "Broadcast to all clients when a queued task is removed before starting (stopped, displaced, expired, or purged)",
			Direction:   "s2c",
			Delivery:    "broadcast",
			Fields: map[string]ManifestField{
				"type":    {Type: "string", UIHint: "hidden"},
				"task_id": {Type: "uuid", Required: true},
				"reason":  {Type: "string", Description: "Why the task was removed: stopped, displaced, expired, purged"},
				"ts":      tsField,
			},
		},
		"actions": {
			Description: "Direct response to a describe request containing action definitions",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":    {Type: "string", UIHint: "hidden"},
				"id":      idEchoField,
				"actions": {Type: "array", Required: true, Items: ManifestField{Ref: "#/types/ActionInfo"}},
			},
		},
		"pool_info_response": {
			Description: "Direct response to a pool_info request or set_pool success",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":   {Type: "string", UIHint: "hidden"},
				"id":     idEchoField,
				"action": {Type: "string", Description: "Action name if scoped; omitted for global"},
				"pool":   {Ref: "#/types/PoolInfo", Required: true},
			},
		},
		"purged": {
			Description: "Direct response to a purge request confirming how many tasks were removed",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":   {Type: "string", UIHint: "hidden"},
				"id":     idEchoField,
				"action": {Type: "string", Description: "Action name if scoped; omitted for all"},
				"count":  {Type: "integer", Required: true, Description: "Number of tasks removed from the queue"},
			},
		},
		"pool_updated": {
			Description: "Broadcast to all clients when pool limits change via set_pool",
			Direction:   "s2c",
			Delivery:    "broadcast",
			Fields: map[string]ManifestField{
				"type":   {Type: "string", UIHint: "hidden"},
				"action": {Type: "string", Description: "Action name if scoped; omitted for global"},
				"pool":   {Ref: "#/types/PoolInfo", Required: true},
			},
		},
		"metrics_response": {
			Description: "Direct response to a metrics request; exactly one of global, action, or task will be non-null",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":   {Type: "string", UIHint: "hidden"},
				"id":     idEchoField,
				"global": {Ref: "#/types/GlobalMetrics", Description: "Global aggregate metrics (when no action or task_id was specified)"},
				"action": {Ref: "#/types/ActionMetrics", Description: "Per-action aggregate metrics"},
				"task":   {Ref: "#/types/TaskMetrics", Description: "Per-task metrics"},
			},
		},
		"subscribed": {
			Description: "Direct confirmation that the connection is now subscribed to the task's events",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":    {Type: "string", UIHint: "hidden"},
				"id":      idEchoField,
				"task_id": {Type: "uuid", Required: true},
			},
		},
		"unsubscribed": {
			Description: "Direct confirmation that the connection is no longer subscribed to the task's events",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":    {Type: "string", UIHint: "hidden"},
				"id":      idEchoField,
				"task_id": {Type: "uuid", Required: true},
			},
		},
		"manifest_response": {
			Description: "Direct response to a manifest request containing the full WS Manifest document",
			Direction:   "s2c",
			Delivery:    "direct",
			Fields: map[string]ManifestField{
				"type":     {Type: "string", UIHint: "hidden"},
				"id":       idEchoField,
				"manifest": {Type: "object", Required: true, Description: "Full WSManifest document (see ws-manifest-spec.md for schema)"},
			},
		},
	}
	return msgs
}

func buildManifestOperations() map[string]ManifestOp {
	return map[string]ManifestOp{
		"start_task": {
			Description: "Start a new task for a registered action",
			Send:        "start",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"queued", "started"},
					Error:   []string{"error"},
				},
				SideEffects: &ManifestOpSideEffects{
					Broadcast:            []string{"queued", "dequeued", "pool_updated"},
					BroadcastSubscribers: []string{"started", "output", "exited", "restarting", "errored"},
				},
			},
			Notes: "The submitting connection is automatically subscribed to the new task's events. " +
				"If the pool is at capacity and queuing is enabled, 'queued' is broadcast immediately; " +
				"'started' arrives when the task actually launches (may be after dequeue). " +
				"'dequeued' arrives if the task is removed before starting.",
		},
		"stop_task": {
			Description: "Stop a running or queued task",
			Send:        "stop",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Error: []string{"error"},
				},
				SideEffects: &ManifestOpSideEffects{
					Broadcast:            []string{"dequeued"},
					BroadcastSubscribers: []string{"exited"},
				},
			},
			Notes: "No direct success message is sent. Confirmation comes via 'exited' broadcast " +
				"(intentional=true) for running tasks, or 'dequeued' broadcast for queued tasks.",
		},
		"reset_task": {
			Description: "Restart an errored task, clearing its exit history",
			Send:        "reset",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"started"},
					Error:   []string{"error"},
				},
			},
			Notes: "Only valid for tasks in the 'errored' state. Bypasses pool limits — " +
				"the task starts immediately regardless of concurrency settings.",
		},
		"list_tasks": {
			Description: "List all known tasks",
			Send:        "list",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"tasks"},
					Error:   []string{"error"},
				},
			},
		},
		"replay_task": {
			Description: "Replay buffered output history for a task",
			Send:        "replay",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"output"},
					Error:   []string{"error"},
				},
			},
			Notes: "Sends zero or more 'output' messages directly to the requesting connection " +
				"(not broadcast). Output is replayed from the task's in-memory ring buffer.",
		},
		"describe_actions": {
			Description: "Describe registered actions and their parameter schemas",
			Send:        "describe",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"actions"},
					Error:   []string{"error"},
				},
			},
		},
		"get_pool_info": {
			Description: "Query current pool state",
			Send:        "pool_info",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"pool_info"},
				},
			},
		},
		"purge_queue": {
			Description: "Remove all queued tasks for an action or globally",
			Send:        "purge",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"purged"},
				},
				SideEffects: &ManifestOpSideEffects{
					Broadcast: []string{"dequeued"},
				},
			},
			Notes: "One 'dequeued' broadcast is sent per removed task.",
		},
		"set_pool_limits": {
			Description: "Dynamically update pool concurrency limits",
			Send:        "set_pool",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"pool_info"},
					Error:   []string{"error"},
				},
				SideEffects: &ManifestOpSideEffects{
					Broadcast: []string{"pool_updated"},
				},
			},
		},
		"subscribe_task": {
			Description: "Subscribe to task-specific broadcast events",
			Send:        "subscribe",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"subscribed"},
					Error:   []string{"error"},
				},
			},
		},
		"unsubscribe_task": {
			Description: "Unsubscribe from task-specific broadcast events",
			Send:        "unsubscribe",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"unsubscribed"},
					Error:   []string{"error"},
				},
			},
		},
		"get_metrics": {
			Description: "Query runtime metrics at global, per-action, or per-task granularity",
			Send:        "metrics",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"metrics"},
				},
			},
			Notes: "Exactly one of 'global', 'action', or 'task' will be non-null in the response. " +
				"Set task_id for per-task metrics, action for per-action metrics, or omit both for global.",
		},
		"get_manifest": {
			Description: "Request the full WS Manifest document for this protocol",
			Send:        "manifest",
			Responses: ManifestOpResponses{
				Direct: &ManifestOpDirect{
					Success: []string{"manifest"},
				},
			},
		},
	}
}

func collectActions(actions map[string]ActionHandler) []ActionInfo {
	if actions == nil {
		return []ActionInfo{}
	}
	infos := make([]ActionInfo, 0, len(actions))
	for name, h := range actions {
		info := h.Describe()
		info.Name = name
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

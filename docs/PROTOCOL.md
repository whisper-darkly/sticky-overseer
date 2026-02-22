# WebSocket Protocol Reference

sticky-overseer exposes a single WebSocket endpoint at `/ws`.
All messages are newline-free JSON objects sent as text frames.

---

## Connection

```
ws://host:port/ws
```

Only connections from trusted networks are accepted (auto-detected local subnets by
default; override via `trusted_cidrs` in config or the `OVERSEER_TRUSTED_CIDRS` env var).

---

## Correlation ID Contract

**Every client→server message must include a non-empty `"id"` string.**

```json
{"type": "list", "id": "req-42"}
```

Rules:

- The server echoes `id` in the corresponding response message.
- A message that omits `id` or sets it to `""` receives an immediate `error` response.
- The connection **remains open** after an `id`-missing error; subsequent valid messages
  are processed normally.
- `id` values are opaque strings — UUIDs, incrementing integers, and short labels all work.
- Server-initiated push events (output, exited, restarting, errored, dequeued,
  pool_updated) carry no `id` because they are not correlated to a specific request.

---

## Message Categories

### Request-Response

The client sends a message and receives exactly one response that echoes the same `id`.

| Client sends | Server responds with |
|---|---|
| `start` | `started` or `queued` or `error` |
| `stop` | _(no direct response; produces `exited` push)_ |
| `reset` | `started` or `error` |
| `list` | `tasks` |
| `replay` | zero or more `output`/`exited` messages (no final ack) |
| `subscribe` | `subscribed` |
| `unsubscribe` | `unsubscribed` |
| `describe` | `actions` |
| `pool_info` | `pool_info` |
| `set_pool` | `pool_info` + `pool_updated` broadcast |
| `purge` | `purged` + `dequeued` broadcasts per cancelled task |
| `metrics` | `metrics` |
| `manifest` | `manifest` |

### Push Events

Sent by the server at any time without a client request.
Subscribers-only events are only sent to connections subscribed to that `task_id`
(including the connection that originally started the task via auto-subscription).
Broadcast events go to all connected clients.

| Message | Delivery |
|---|---|
| `output` | subscribers only |
| `exited` | subscribers only |
| `restarting` | subscribers only |
| `errored` | subscribers only |
| `dequeued` | broadcast |
| `pool_updated` | broadcast |

---

## Client → Server Messages

### `start`

Start a named action as a new task.

```json
{
  "type": "start",
  "id": "req-1",
  "action": "build",
  "task_id": "build-abc",
  "params": {"branch": "main"},
  "retry_policy": {"restart_delay": "5s", "error_threshold": 3, "error_window": "60s"},
  "force": false
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | Correlation ID |
| `action` | string | **yes** | Registered action name |
| `task_id` | string | no | Client-assigned task ID; UUID generated if omitted |
| `params` | object (string→string) | no | Action parameters |
| `retry_policy` | object | no | Overrides action-level retry policy |
| `force` | bool | no | Force-start even when pool is at capacity (displaces lowest-priority queued item) |

Responses:
- `started` — task launched immediately
- `queued` — task accepted but waiting for a pool slot
- `error` — unknown action, validation failure, or duplicate task_id

Auto-subscription: the submitting connection is automatically subscribed to events for
the new task_id.

### `stop`

Stop a running or queued task.

```json
{"type": "stop", "id": "req-2", "task_id": "build-abc"}
```

Sends SIGTERM to the process group. If the process has not exited after 5 s, SIGKILL is
sent. For a queued (not yet running) task, the task is removed from the queue and a
`dequeued` broadcast is sent. No direct acknowledgement is sent; the task will produce
an `exited` push event.

### `reset`

Clear an `errored` task back to `active` and restart it immediately.

```json
{"type": "reset", "id": "req-3", "task_id": "build-abc"}
```

Error if the task is not in `errored` state.

### `list`

List all known tasks.

```json
{"type": "list", "id": "req-4", "since": "2024-01-01T00:00:00Z"}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `since` | RFC3339 string | no | Only return tasks whose last activity is after this time |

Response: `tasks` message containing a `tasks` array of `TaskInfo` objects. Also includes
tasks currently sitting in the pool queue (state `"queued"`).

### `replay`

Replay buffered events from a task's ring buffer (last 100 events).

```json
{"type": "replay", "id": "req-5", "task_id": "build-abc", "since": "2024-01-01T00:00:00Z"}
```

Sends zero or more `output` / `exited` messages directly to the requesting connection.
No terminating acknowledgement is sent.

### `subscribe` / `unsubscribe`

Manage subscriptions to task-specific push events.

```json
{"type": "subscribe",   "id": "req-6", "task_id": "build-abc"}
{"type": "unsubscribe", "id": "req-7", "task_id": "build-abc"}
```

A connection is automatically subscribed when it starts a task. Manual subscription
allows a second connection (e.g. a UI reconnect) to receive events for an existing task.

### `describe`

Introspect registered action drivers.

```json
{"type": "describe", "id": "req-8"}
{"type": "describe", "id": "req-9", "action": "build"}
```

Returns all actions if `action` is omitted, or a single-element list if specified.

### `pool_info`

Query pool concurrency state.

```json
{"type": "pool_info", "id": "req-10", "action": "build"}
```

Omit `action` to query the global pool. Response includes current running count, queue
depth, and individual queued task IDs.

### `set_pool`

Resize pool limits at runtime.

```json
{
  "type": "set_pool",
  "id": "req-11",
  "action": "build",
  "size": 4,
  "queue_size": 20,
  "excess": {"action": "stop", "order": "first", "grace": 5}
}
```

Broadcasts `pool_updated` to all clients after applying the change.

### `purge`

Drain all queued (not yet running) tasks for an action.

```json
{"type": "purge", "id": "req-12", "action": "build"}
```

Omit `action` to purge all queues. Each cancelled task generates a `dequeued` broadcast.
Response: `purged` with a `count` of removed items.

### `metrics`

Query in-memory counters. Exactly one of `action` or `task_id` scopes the response;
if neither is provided the global snapshot is returned.

```json
{"type": "metrics", "id": "req-13"}
{"type": "metrics", "id": "req-14", "action": "build"}
{"type": "metrics", "id": "req-15", "task_id": "build-abc"}
```

### `manifest`

Retrieve the full WS Manifest protocol descriptor.

```json
{"type": "manifest", "id": "req-16"}
```

Also available via HTTP `GET /ws/manifest`.

---

## Server → Client Messages

### `started`

```json
{
  "type": "started",
  "id": "req-1",
  "task_id": "build-abc",
  "pid": 12345,
  "restart_of": 0,
  "ts": "2024-01-01T12:00:00Z"
}
```

`restart_of` is the PID of the previous process when this is an automatic restart (0 on
first start). `id` echoes the originating `start` or `reset` request; it is `""` when
the server auto-restarts a task (retry policy).

### `tasks`

```json
{
  "type": "tasks",
  "id": "req-4",
  "tasks": [
    {
      "task_id": "build-abc",
      "action": "build",
      "params": {"branch": "main"},
      "state": "active",
      "current_pid": 12345,
      "worker_state": "running",
      "restart_count": 0,
      "created_at": "2024-01-01T12:00:00Z",
      "last_started_at": "2024-01-01T12:00:00Z"
    }
  ]
}
```

`state` values: `"active"`, `"stopped"`, `"errored"`, `"queued"`.

### `output`

```json
{
  "type": "output",
  "task_id": "build-abc",
  "pid": 12345,
  "stream": "stdout",
  "data": "Building...",
  "seq": 42,
  "ts": "2024-01-01T12:00:01Z"
}
```

`stream`: `"stdout"` or `"stderr"`. `seq` is a per-task monotonic counter starting at 1.
After reconnection, compare the first received `seq` against the last seen value to
detect how many ring-buffer events were missed.

### `exited`

```json
{
  "type": "exited",
  "task_id": "build-abc",
  "pid": 12345,
  "exit_code": 0,
  "intentional": true,
  "ts": "2024-01-01T12:00:05Z"
}
```

`intentional` is `true` when the exit was caused by a `stop` request.

### `restarting`

```json
{
  "type": "restarting",
  "task_id": "build-abc",
  "pid": 12345,
  "restart_delay": "5s",
  "attempt": 1,
  "ts": "2024-01-01T12:00:06Z"
}
```

Sent immediately after an unintentional exit when a retry policy is configured and the
error threshold has not been reached.

### `errored`

```json
{
  "type": "errored",
  "task_id": "build-abc",
  "pid": 12345,
  "exit_count": 3,
  "ts": "2024-01-01T12:00:30Z"
}
```

Sent when the task's exit count within `error_window` meets or exceeds `error_threshold`.
The task transitions to `errored` state and will not restart automatically. Use `reset`
to resume.

### `queued`

```json
{
  "type": "queued",
  "id": "req-1",
  "task_id": "build-xyz",
  "action": "build",
  "position": 3,
  "ts": "2024-01-01T12:01:00Z"
}
```

Sent when a `start` request is accepted but no pool slot is currently available.

### `dequeued`

```json
{
  "type": "dequeued",
  "task_id": "build-xyz",
  "reason": "stopped",
  "ts": "2024-01-01T12:01:30Z"
}
```

`reason` values: `"stopped"` (explicit `stop`), `"purged"` (explicit `purge`),
`"displaced"` (a higher-priority task took the slot), `"expired"` (queue timeout).

### `subscribed` / `unsubscribed`

```json
{"type": "subscribed",   "id": "req-6", "task_id": "build-abc"}
{"type": "unsubscribed", "id": "req-7", "task_id": "build-abc"}
```

### `pool_info`

```json
{
  "type": "pool_info",
  "id": "req-10",
  "action": "build",
  "pool": {
    "limit": 4,
    "running": 2,
    "queue_depth": 1,
    "queue_items": [{"task_id": "build-xyz", "action": "build", "queued_at": "..."}]
  }
}
```

### `purged`

```json
{"type": "purged", "id": "req-12", "action": "build", "count": 5}
```

### `pool_updated`

```json
{
  "type": "pool_updated",
  "action": "build",
  "pool": {"limit": 4, "running": 2, "queue_depth": 0}
}
```

Broadcast to all clients when pool limits change via `set_pool`.

### `metrics`

```json
{
  "type": "metrics",
  "id": "req-13",
  "global": {
    "tasks_started": 100,
    "tasks_completed": 95,
    "tasks_errored": 2,
    "tasks_restarted": 8,
    "total_output_lines": 48200,
    "enqueued": 12,
    "dequeued": 12,
    "displaced": 1,
    "expired": 0
  }
}
```

Exactly one of `global`, `action`, or `task` is non-null.

### `error`

```json
{
  "type": "error",
  "id": "req-1",
  "message": "unknown action: \"typo\"",
  "existing_task_id": ""
}
```

`existing_task_id` is set when the error is a deduplication rejection (a task with
identical dedup-key params is already running). The connection stays open.

---

## Task State Machine

```
         start (accepted)
              │
              ▼
          [ active ] ◄──────────────────────┐
         /    │    \                         │
        /     │     \                        │  restart (retry policy)
       ▼      │      ▼                       │
  running  queued  restarting ───────────────┘
       │
  exit (intentional)     exit (unintentional, threshold not reached)
       │                          │
       ▼                          ▼
  [ stopped ]               [ restarting ] ──► [ active ]
                                  │
                        threshold exceeded
                                  │
                                  ▼
                            [ errored ] ──► reset ──► [ active ]
```

State transitions:
- `active` → `stopped` via `stop` request or intentional `exited`
- `active` → `errored` when exit count within `error_window` ≥ `error_threshold`
- `errored` → `active` via `reset` request

---

## RetryPolicy

```json
{
  "restart_delay": "5s",
  "error_threshold": 3,
  "error_window": "60s"
}
```

| Field | Type | Description |
|---|---|---|
| `restart_delay` | duration string | Wait this long before restarting after an exit (default `"0s"`) |
| `error_threshold` | int | Max non-intentional exits within `error_window` before transitioning to `errored` (0 = no limit) |
| `error_window` | duration string | Sliding time window for counting exits (0 = all-time) |

Duration strings use Go format: `"5s"`, `"1m30s"`, `"2h"`.

Exit history within `error_window` is tracked in-memory only and resets on overseer
restart. This is intentional — a restart is a valid way to clear runaway tasks.

---

## Pool Semantics

Each action has an independent pool governed by `PoolConfig`. A global default applies to
actions without an explicit config.

```yaml
task_pool:
  size: 4           # max concurrent running tasks (0 = unlimited)
  queue_size: 20    # max tasks waiting in queue (0 = no queue)
  queue_timeout: "30s"  # how long a task may wait before expiry (0 = indefinite)
```

**Excess policy** (`force: true` or `set_pool` size reduction):

```yaml
excess:
  action: stop      # "stop" running tasks to make room, or "requeue" them
  order: first      # "first" = stop longest-running; "last" = stop shortest-running
  grace: 5          # seconds between SIGTERM and SIGKILL
```

**Queue ordering**:
- `order: first` (default) — FIFO
- `order: last` — LIFO

**Displacement**: when `displace: true` and the queue is full, the lowest-priority queued
item is removed to make room for the new arrival.

---

## Subscription Model

- A connection that sends `start` is **automatically subscribed** to that task.
- Any connection can manually subscribe/unsubscribe at any time.
- Subscriptions are connection-scoped and lost on disconnect.
- Output events, lifecycle events (exited, restarting, errored), and the started
  confirmation are all delivered only to subscribers.
- `dequeued` and `pool_updated` are broadcast to all connections regardless of subscription.

---

## Output Filtering (exec driver)

The built-in `exec` driver supports per-stream CEL filter expressions that drop lines
before they reach clients.

```yaml
actions:
  build:
    type: exec
    config:
      entrypoint: make
      stdout_condition: 'output.data.contains("error") || output.data.contains("warning")'
      stderr_condition: 'true'
```

CEL variables available in filter expressions:

| Variable | Type | Description |
|---|---|---|
| `output.stream` | string | `"stdout"` or `"stderr"` |
| `output.data` | string | The raw line text |
| `output.json` | map | Parsed JSON if the line is valid JSON, else `{}` |

---

## CEL Parameter Validation

Parameter specs in the YAML config may include a `validate` CEL expression:

```yaml
parameters:
  branch:
    default: "main"
    validate: 'value.matches("^[a-zA-Z0-9/_-]+$")'
```

The expression receives `value` (the string parameter value) and must return `bool`.
Validation runs at `start` time before any process is launched.

---

## Template Rendering (exec driver)

Command arguments support `[[ ]]`-delimited Go templates. The delimiters were chosen to
avoid conflicts with shell `${}` and JSON `{}`.

```yaml
config:
  entrypoint: git
  command: ["clone", "[[.repo]]", "--branch", "[[.branch]]"]
```

All task `params` are available as template variables. Missing keys render as empty string.

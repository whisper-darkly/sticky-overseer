# sticky-overseer

WebSocket-based process overseer — spawn, track, and stream output from child processes in real-time. Ships as both an **importable Go library** and a **pre-built binary**.

## As a library

Downstream projects import sticky-overseer and register custom action drivers:

```go
import (
    overseer "github.com/whisper-darkly/sticky-overseer/v2"
    _ "github.com/whisper-darkly/sticky-overseer/v2/exec"  // built-in exec driver
    _ "myorg/drivers/converter"                             // custom driver
)

var version = "dev"
var commit  = "unknown"

func main() { overseer.RunCLI(version, commit) }
```

Drivers self-register at `init()` time (the database driver pattern). See the `exec/` package
for a working reference implementation.

## Build

```bash
make build        # → dist/sticky-overseer
make install      # → /usr/local/bin/sticky-overseer
```

## Install from source

```bash
go install github.com/whisper-darkly/sticky-overseer/v2/cmd/sticky-overseer@latest
```

## Usage

```bash
./sticky-overseer --config ./config.yaml
./sticky-overseer --list-handlers    # print registered action driver types
./sticky-overseer --version
```

## Configuration

`config.yaml`:

```yaml
listen: ":8080"        # TCP address, or "unix:/path/to/sock" for Unix socket
db: overseer.db        # SQLite path for exit-history (optional)
trusted_cidrs: ""      # Comma-separated IPs/CIDRs; empty = auto-detect local subnets
log_file: ""           # Optional JSONL event log

# Global pool defaults (overridable per-action)
task_pool:
  size: 10             # max concurrent tasks (0 = unlimited)
  queue_size: 100      # max queued tasks
  queue_timeout: "0s"  # 0 = queue indefinitely

retry: {}              # global retry policy; overridable per-action

actions:
  echo:
    type: exec
    config:
      entrypoint: echo
      command: ["[[.message]]"]
      parameters:
        message:
          default: "hello"
```

### Environment overrides

| Variable | Description |
|---|---|
| `OVERSEER_CONFIG` | Path to YAML config (overrides `--config`) |
| `OVERSEER_LISTEN` | Listen address (overrides `config.listen`) |
| `OVERSEER_DB` | SQLite path (overrides `config.db`) |
| `OVERSEER_LOG_FILE` | JSONL event log path |

## WebSocket Protocol

Connect at `ws://host:8080/ws`. All messages are JSON.

> **Correlation ID contract**: every client→server message **must** include a non-empty `id`
> string. The server echoes `id` back in the corresponding response. Messages without `id`
> receive an `error` response and the connection stays open.

### Client → Server

| `type` | Required fields | Optional fields | Description |
|--------|----------------|-----------------|-------------|
| `start` | `id`, `action` | `task_id`, `params`, `retry_policy`, `force` | Start a task; `task_id` auto-generated if omitted |
| `stop` | `id`, `task_id` | | SIGTERM → SIGKILL escalation |
| `reset` | `id`, `task_id` | | Clear errored state and restart |
| `list` | `id` | `since` (RFC3339) | List tasks |
| `replay` | `id`, `task_id` | `since` (RFC3339) | Replay ring-buffer to caller |
| `subscribe` | `id`, `task_id` | | Subscribe to a task's events |
| `unsubscribe` | `id`, `task_id` | | Unsubscribe |
| `describe` | `id` | `action` | List registered actions; filter by `action` if given |
| `pool_info` | `id` | `action` | Pool state for one or all actions |
| `set_pool` | `id`, `action`, `size` | | Resize a pool at runtime |
| `purge` | `id` | `action` | Drain the queue for an action |
| `metrics` | `id` | `action`, `task_id` | In-memory metrics at global/action/task granularity |
| `manifest` | `id` | | Retrieve the WS Manifest protocol descriptor |

### Server → Client

Push events carry no `id` (they are not correlated to a single request).
Request-response messages echo the `id` from the originating request.

| `type` | `id` | Key fields | Notes |
|--------|------|-----------|-------|
| `started` | echoed | `task_id`, `pid`, `restart_of`, `ts` | Also sent on auto-restart; `restart_of` is previous PID |
| `tasks` | echoed | `tasks[]` | Response to `list` |
| `actions` | echoed | `actions[]` | Response to `describe` |
| `pool_info` | echoed | `action`, `pool` | Response to `pool_info` / `set_pool` |
| `purged` | echoed | `action`, `count` | Response to `purge` |
| `subscribed` | echoed | `task_id` | Confirms `subscribe` |
| `unsubscribed` | echoed | `task_id` | Confirms `unsubscribe` |
| `metrics` | echoed | `global` / `action` / `task` | Response to `metrics` |
| `manifest` | echoed | `manifest` | Response to `manifest` |
| `error` | echoed | `message`, `existing_task_id`? | Error response; connection stays open |
| `queued` | echoed | `task_id`, `action`, `position`, `ts` | Task accepted into pool queue |
| `output` | — | `task_id`, `pid`, `stream`, `data`, `seq`, `ts` | Push; subscribers only |
| `exited` | — | `task_id`, `pid`, `exit_code`, `intentional`, `ts` | Push; subscribers only |
| `restarting` | — | `task_id`, `pid`, `restart_delay`, `attempt`, `ts` | Push; subscribers only |
| `errored` | — | `task_id`, `pid`, `exit_count`, `ts` | Push; subscribers only |
| `dequeued` | — | `task_id`, `reason`, `ts` | Push; broadcast to all clients |
| `pool_updated` | — | `action`, `pool` | Push; broadcast on `set_pool` |

Output events carry a per-task monotonically increasing `seq` counter. Clients can detect
ring-buffer gaps after reconnection by comparing `seq` values.

## Writing a custom driver

Implement `overseer.ActionHandler` and `overseer.ActionHandlerFactory`, register via `init()`:

```go
package mydriver

import (
    "context"
    overseer "github.com/whisper-darkly/sticky-overseer/v2"
)

type myHandler struct { cfg myConfig }

func (h *myHandler) Describe() overseer.ActionInfo { ... }
func (h *myHandler) Validate(params map[string]string) error { ... }
func (h *myHandler) Start(taskID string, params map[string]string, cb overseer.WorkerCallbacks) (*overseer.Worker, error) {
    // Option A: wrap an OS process
    return overseer.StartWorker(overseer.WorkerConfig{
        TaskID:        taskID,
        Command:       "my-tool",
        Args:          []string{params["input"]},
        IncludeStdout: true,
        IncludeStderr: true,
    }, cb)

    // Option B: wrap a Go goroutine
    return overseer.StartVirtualWorker(taskID, func(ctx context.Context, send func(overseer.Stream, string)) int {
        send(overseer.StreamStdout, "working...")
        // ... do work, check ctx.Done() periodically ...
        return 0
    }, cb)
}

type myFactory struct{}
func (f *myFactory) Type() string { return "mydriver" }
func (f *myFactory) Create(config map[string]any, name string, retry overseer.RetryPolicy, pool overseer.PoolConfig, dedupe []string) (overseer.ActionHandler, error) {
    // parse config map → myConfig
    return &myHandler{cfg: parsed}, nil
}

func init() { overseer.RegisterFactory(&myFactory{}) }
```

### Background service drivers

Drivers that need a long-running background goroutine (e.g. a directory watcher that
auto-discovers work) implement `overseer.ServiceHandler`:

```go
// RunService is called once at startup; blocks until ctx is cancelled.
func (h *myHandler) RunService(ctx context.Context, submit overseer.TaskSubmitter) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // Discover work and submit tasks programmatically.
            submit.Submit("mydriver", "", map[string]string{"file": "/path/to/file"})
        }
    }
}
```

## Protocol Manifest

`GET /ws/manifest` returns a machine-readable JSON document that describes the
entire WebSocket protocol — every message type, field, delivery mode, and
logical operation — conforming to the
[WS Manifest schema](https://github.com/whisper-darkly/sticky-bb/blob/main/docs/ws-manifest-spec.md).

This enables generic UI tools (such as [sticky-bb](https://github.com/whisper-darkly/sticky-bb))
to automatically build typed send forms and response panels without any hardcoded
knowledge of the overseer protocol.

The same document is also available over the WebSocket connection:

```json
{"type": "manifest", "id": "req-1"}
```

### curl example

```bash
# List all operation names
curl -s http://localhost:8080/ws/manifest | jq '.operations | keys'

# See fields for the start message
curl -s http://localhost:8080/ws/manifest | jq '.messages.start.fields'

# List configured actions
curl -s http://localhost:8080/ws/manifest | jq '.actions[].name'
```

### sticky-bb integration

See `docker/compose-bb.yaml` for a complete Docker Compose example pairing
sticky-overseer with sticky-bb for a full browser-based testing UI.

## Docker

```bash
make -C docker build    # build image
make -C docker export   # export as .tar
docker run -p 8080:8080 sticky-overseer:latest
```

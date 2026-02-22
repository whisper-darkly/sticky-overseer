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

Connect at `ws://host:8080/ws`. All messages are JSON. The optional `id` field echoes back in responses for request/response correlation.

### Client → Server

| `type` | Key fields | Description |
|--------|-----------|-------------|
| `start` | `action`, `task_id`, `params`, `retry_policy`, `force`, `id` | Start a task |
| `stop` | `task_id`, `id` | SIGTERM → SIGKILL escalation |
| `reset` | `task_id`, `id` | Clear errored state and restart |
| `list` | `since` (RFC3339), `id` | List tasks |
| `replay` | `task_id`, `since`, `id` | Replay ring-buffer to caller |
| `subscribe` | `task_id` | Subscribe to a task's output |
| `unsubscribe` | `task_id` | Unsubscribe |
| `describe` | `id` | List registered actions and their parameters |
| `pool_info` | `action`, `id` | Pool state for one or all actions |
| `set_pool` | `action`, `size`, `id` | Resize a pool at runtime |
| `purge` | `action`, `id` | Drain the queue for an action |
| `metrics` | `action`, `task_id`, `id` | In-memory metrics |

### Server → Client

| `type` | Description |
|--------|-------------|
| `started` | Worker spawned (`task_id`, `pid`, `ts`) |
| `output` | Stdout/stderr line (`task_id`, `stream`, `data`, `seq`, `ts`) |
| `exited` | Worker exited (`task_id`, `exit_code`, `intentional`, `ts`) |
| `restarting` | Retry scheduled (`task_id`, `attempt`, `restart_delay`, `ts`) |
| `errored` | Retry threshold exceeded (`task_id`, `exit_count`, `ts`) |
| `queued` | Task queued in pool (`task_id`, `position`, `ts`) |
| `dequeued` | Queued task cancelled (`task_id`, `reason`, `ts`) |
| `tasks` | Response to `list` |
| `actions` | Response to `describe` |
| `pool_info` | Response to `pool_info` |
| `metrics` | Response to `metrics` |
| `error` | Error response (`message`, `id`) |

Output events include a monotonically increasing `seq` number per task. Clients can detect
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

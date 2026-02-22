# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build                    # compile to dist/sticky-overseer (from ./cmd/sticky-overseer)
make install                  # build + install to /usr/local/bin
make clean                    # remove dist/
go test -race ./...           # run root package tests
go test -race ./... ./exec/... # run all tests including exec/ sub-package
go vet ./...                  # static analysis
```

Run directly after building:
```bash
./dist/sticky-overseer --config ./config.yaml
./dist/sticky-overseer --list-handlers   # print registered driver types
OVERSEER_LISTEN=:9090 ./dist/sticky-overseer
```

## Architecture

This project is an **importable Go library** (`package overseer`) plus a thin binary wrapper.

### Package layout

```
sticky-overseer/          ← package overseer  (importable library)
  hub.go                  ← Hub: task orchestration, client connections, subscriptions
  pool.go                 ← PoolManager: concurrency limits, queuing, displacement
  worker.go               ← Worker: OS-process wrapper + virtual goroutine worker
  transport.go            ← TCP/Unix socket transports; WebSocket upgrade
  messages.go             ← all JSON message types (both directions)
  store.go                ← SQLite: OpenDB helper (WAL mode); no persistent tables
  config.go               ← YAML config loading; ActionConfig, ParamSpec, RetryPolicy
  actions.go              ← ActionHandler/Factory interfaces, CEL helpers, ServiceHandler
  run.go                  ← RunCLI() entry point
  metrics.go              ← in-memory metrics
  *_test.go               ← package overseer white-box tests

  exec/                   ← package exec  (built-in exec driver; blank-importable)
    handler.go            ← ExecHandler: runs OS processes with template rendering
    handler_test.go

  cmd/sticky-overseer/    ← package main  (thin binary wrapper)
    main.go               ← imports overseer + _ exec; calls overseer.RunCLI()

  docker/                 ← playground HTML (embedded in transport.go)
  docs/                   ← swagger.json (embedded in transport.go)
```

### Key design points

**Library / plugin architecture**: Downstream binaries import this library and blank-import
driver packages to register them at `init()` time (the database driver pattern). The `exec/`
package is both the built-in driver and the canonical reference implementation.

**ActionHandler** (`actions.go`): the core interface. Per-task drivers implement:
- `Describe() ActionInfo` — metadata for introspection
- `Validate(params) error` — param validation (CEL expressions supported)
- `Start(taskID, params, cb) (*Worker, error)` — launch one worker per task

**ServiceHandler** (`actions.go`): optional extension for drivers that need a background
goroutine (e.g. a directory scanner). The Hub calls `RunService(ctx, submit)` once at startup.
`TaskSubmitter.Submit(action, taskID, params)` enqueues work programmatically.

**ActionHandlerFactory** (`actions.go`): registered at `init()` time via
`overseer.RegisterFactory()`. `BuildActionHandlers()` calls `Create()` for each action in
the YAML config.

**Worker** (`worker.go`):
- OS-process workers: `StartWorker(cfg, cb)` — wraps `exec.Cmd`, sets `Setpgid: true`
  for process-group kill, buffers up to 10MB lines, stores last 100 events in a ring buffer.
- Virtual (goroutine) workers: `StartVirtualWorker(taskID, fn, cb)` — wraps a Go function;
  `Stop()` cancels the context passed to `fn`.
- `WorkerCallbacks{OnOutput, LogEvent, OnExited}` decouple Worker from Hub.
- `Stop()` sends SIGTERM to process group; escalates to SIGKILL after 5 s.

**Hub** (`hub.go`):
- `tasks` map (keyed by task_id) holds in-memory `TaskRecord` + current `*Worker`.
- `subscriptions` map tracks per-connection task subscriptions.
- `pending` map prevents TOCTOU duplicate-start races.
- `Hub.Submit(action, taskID, params)` implements `TaskSubmitter` for ServiceHandlers.
- Broadcast: `Broadcast(msg)` for global events; `BroadcastToSubscribers(taskID, msg)`
  for task-specific events (output, exited, restarting, errored, started).
- Auto-subscribes the submitting WebSocket connection to its started task.
- Exit history is tracked in-memory only; resets on overseer restart by design.

**PoolManager** (`pool.go`):
- Per-action concurrency limits with a global default.
- Queue with configurable size and timeout.
- Displacement of lower-priority queued items.
- `Acquire(taskID, action, force, stopFn, startFn, cancelFn)` returns `AcquireRunning`
  or `AcquireQueued`; `startFn` is called outside pm.mu to avoid lock-order inversions.

**RetryPolicy** (`messages.go`): drives automatic restarts —
`RestartDelay`, `ErrorWindow` (sliding window), `ErrorThreshold` (max exits within window).

**Task states**: `active` (running or awaiting restart) → `stopped` (intentional) → `errored`
(exit threshold exceeded).

**CEL validation** (`actions.go`): parameter `validate` fields and output `condition` fields
are compiled at `Create()` time using Google CEL. Parameter expr variable: `value`.
Output filter expr variables: `output.stream`, `output.data`, `output.json`.

**Output filtering** (`exec/handler.go`): per-stream CEL conditions on stdout/stderr.
Non-matching lines are dropped before reaching WebSocket clients.

**Template rendering** (`exec/handler.go`): entrypoint and command args support
`[[ ]]`-delimited Go templates (chosen to avoid conflicts with shell `${}` and JSON `{}`).

### WebSocket protocol

All messages are JSON. `IncomingMessage` (client→server) dispatches on `type`.

**Client → Server**:

| type | fields | notes |
|------|--------|-------|
| `start` | `action`, `task_id`, `params`, `retry_policy`, `force`, `id` | `task_id` auto-generated if absent |
| `stop` | `task_id`, `id` | SIGTERM → SIGKILL after 5 s |
| `reset` | `task_id`, `id` | clears errored state, restarts |
| `list` | `since` (RFC3339), `id` | filtered task list |
| `replay` | `task_id`, `since`, `id` | ring-buffer replay to caller |
| `subscribe` | `task_id`, `id` | subscribe to task events |
| `unsubscribe` | `task_id`, `id` | unsubscribe |
| `describe` | `id` | list registered actions |
| `pool_info` | `action`, `id` | pool state |
| `set_pool` | `action`, `size`, `id` | resize pool |
| `purge` | `action`, `id` | drain queue |
| `metrics` | `action`, `task_id`, `id` | in-memory metrics |
| `manifest` | `id` | full WS Manifest protocol descriptor |

**Server → Client**:

Push events (no `id`): `output`, `exited`, `restarting`, `errored`, `dequeued`, `pool_updated`.
Request-response messages (echo `id`): everything else.

| type | fields |
|------|--------|
| `started` | `task_id`, `pid`, `restart_of`, `ts`, `id` |
| `tasks` | `tasks[]` (TaskInfo), `id` |
| `output` | `task_id`, `pid`, `stream`, `data`, `seq`, `ts` |
| `exited` | `task_id`, `pid`, `exit_code`, `intentional`, `ts` |
| `restarting` | `task_id`, `pid`, `restart_delay`, `attempt`, `ts` |
| `errored` | `task_id`, `pid`, `exit_count`, `ts` |
| `queued` | `task_id`, `action`, `position`, `ts`, `id` |
| `dequeued` | `task_id`, `reason`, `ts` |
| `actions` | `actions[]` (ActionInfo), `id` |
| `pool_info` | pool state, `action`, `id` |
| `purged` | `action`, `count`, `id` |
| `pool_updated` | `action`, `pool` |
| `subscribed` | `task_id`, `id` |
| `unsubscribed` | `task_id`, `id` |
| `metrics` | metrics data, `id` |
| `manifest` | manifest doc, `id` |
| `error` | `message`, `id` |

`seq` is a per-task monotonic counter on each output event.
Reconnecting clients can detect ring-buffer gaps by comparing `seq` values.

Every client→server message **must** include a non-empty `id`. Messages missing `id` receive
an `error` response; the connection remains open. See `docs/PROTOCOL.md` for full contracts.

### Consumer pattern (how downstream binaries use this library)

```go
package main

import (
    overseer "github.com/whisper-darkly/sticky-overseer/v2"
    _ "github.com/whisper-darkly/sticky-overseer/v2/exec"  // built-in handler
    _ "myorg/drivers/converter"                             // custom handler
)

var version = "dev"
var commit  = "unknown"

func main() { overseer.RunCLI(version, commit) }
```

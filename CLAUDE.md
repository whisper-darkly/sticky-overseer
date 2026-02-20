# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build       # compile to dist/sticky-overseer
make install     # build + install to /usr/local/bin
make clean       # remove dist/
go test -race ./...  # run tests
go vet ./...     # static analysis
```

Run directly after building:
```bash
./dist/sticky-overseer                        # default port 8080
./dist/sticky-overseer -command /path/to/bin  # pin allowed command
OVERSEER_PORT=9090 ./dist/sticky-overseer
```

## Architecture

Single-package Go binary (`package main`) with five files:

- **main.go** — entry point; parses flags/env, opens SQLite DB, sets up IP allowlist, registers `/ws` HTTP handler, handles graceful shutdown
- **hub.go** — `Hub` manages connected WebSocket clients and persistent `Task` records; dispatches incoming JSON messages (`start`/`list`/`stop`/`reset`/`replay`), broadcasts outbound events, implements retry/restart logic
- **worker.go** — `Worker` wraps `exec.Cmd`; pipes stdout/stderr via goroutines, stores events in a 100-slot ring buffer, sends SIGTERM on stop (SIGKILL after 5s); callbacks decouple Worker from Hub
- **store.go** — SQLite persistence via `openDB`; schema v2; WAL mode + 5 s busy timeout; stores task records including retry state and exit timestamps
- **messages.go** — all JSON struct types for the WebSocket protocol (both directions) plus `RetryPolicy` with duration-string marshalling

### Key design points

- Tasks are keyed by `task_id` (UUID) in `Hub.tasks`. A `Task` holds a `TaskRecord` (DB state), the current `*Worker`, and an `exitHistory` slice for retry-window tracking.
- Task states: `active` → running or awaiting restart; `stopped` → intentionally stopped; `errored` → exit threshold exceeded.
- `RetryPolicy` on a task drives automatic restarts: `restart_delay` (duration), `error_window` (sliding window), `error_threshold` (max exits within window before entering errored state).
- `Hub.Broadcast` sends to **all** connected clients when a worker produces output or exits — there is no per-client subscription model. Each connection has its own write lock to prevent concurrent writes.
- The ring buffer (`Worker.events`) holds the last 100 events. `replay` replays from this buffer to a single client; useful for reconnecting clients.
- `pinnedCommand` restricts `start` commands to a single executable. When set, clients may omit the command field or must match exactly.
- IP allowlisting is enforced at WebSocket upgrade time. Default: loopback + all local interface subnets. Override with `OVERSEER_TRUSTED_CIDRS`.
- `OVERSEER_LOG_FILE` opens a JSONL event log; all `started`, `output`, and `exited` events are appended.
- Concurrency: `Hub.mu` (RWMutex) guards `clients` and `tasks` maps. `Task.mu` (Mutex) guards per-task state. `Worker.mu` (Mutex) guards worker state and events ring buffer.

### WebSocket protocol

All messages are JSON. Client→server messages share `IncomingMessage`; server→client messages are typed individually.

**Client → Server** (`type` field selects handler):

| type | fields | notes |
|------|--------|-------|
| `start` | `task_id`, `command`, `args[]`, `retry_policy`, `id` | `command` may be omitted when pinned; `task_id` auto-generated if absent |
| `list` | `since` (RFC3339), `id` | filters tasks by last activity time |
| `stop` | `task_id`, `id` | sends SIGTERM to worker; SIGKILL after 5 s |
| `reset` | `task_id`, `id` | clears errored state and restarts worker |
| `replay` | `task_id`, `since` (RFC3339), `id` | replays ring buffer to caller only |

**Server → Client** (`type` field):

| type | fields |
|------|--------|
| `started` | `task_id`, `pid`, `restart_of`, `ts`, `id` |
| `tasks` | `tasks[]` (TaskInfo), `id` |
| `output` | `task_id`, `pid`, `stream` (stdout/stderr), `data`, `ts` |
| `exited` | `task_id`, `pid`, `exit_code`, `intentional`, `ts` |
| `restarting` | `task_id`, `pid`, `restart_delay`, `attempt`, `ts` |
| `errored` | `task_id`, `pid`, `exit_count`, `ts` |
| `error` | `message`, `id` |

The optional `id` field in client messages is echoed back in the response, allowing request/response correlation.

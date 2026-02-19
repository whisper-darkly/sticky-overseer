# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build       # compile to dist/sticky-overseer
make install     # build + install to /usr/local/bin
make clean       # remove dist/
go test ./...    # run tests (none currently)
go vet ./...     # static analysis
```

Run directly after building:
```bash
./dist/sticky-overseer                        # default port 8080
./dist/sticky-overseer -command /path/to/bin  # pin allowed command
OVERSEER_PORT=9090 ./dist/sticky-overseer
```

## Architecture

Single-package Go binary (`package main`) with four files:

- **main.go** — entry point; parses flags/env, sets up IP allowlist, registers `/ws` HTTP handler
- **hub.go** — `Hub` manages connected WebSocket clients and running workers; dispatches incoming JSON messages (`start`/`list`/`stop`/`replay`) and broadcasts outbound events to all clients
- **worker.go** — `Worker` wraps `exec.Cmd`; pipes stdout/stderr via goroutines, stores events in a 100-slot ring buffer, sends SIGTERM on stop (SIGKILL after 5s)
- **messages.go** — all JSON struct types for the WebSocket protocol (both directions)

### Key design points

- Workers are keyed by PID in `Hub.workers`. PIDs are never reused within a session, so PID is the stable identifier used in all client commands.
- `Hub.Broadcast` sends to **all** connected clients when a worker produces output or exits — there is no per-client subscription model.
- The ring buffer (`Worker.events`) holds the last 100 events. `replay` replays from this buffer to a single client; useful for reconnecting clients.
- `pinnedCommand` restricts `start` commands to a single executable. When set, clients may omit the command field or must match exactly.
- IP allowlisting is enforced at WebSocket upgrade time. Default: loopback + all local interface subnets. Override with `OVERSEER_TRUSTED_CIDRS`.
- `OVERSEER_LOG_FILE` env var is accepted by the binary (shown in `-help`) but not yet implemented — the flag exists, the file-writing logic does not.
- Concurrency: `Hub.mu` (RWMutex) guards both `clients` and `workers` maps. `Broadcast` holds `RLock` while calling `WriteMessage` on each connection — there is no per-connection write lock, so concurrent broadcasts can race on a single conn. `Worker.mu` is a separate `sync.Mutex` guarding only `Worker.events` and state fields.

### WebSocket protocol

All messages are JSON. Client→server messages share `IncomingMessage`; server→client messages are typed individually.

**Client → Server** (`type` field selects handler):

| type | fields | notes |
|------|--------|-------|
| `start` | `command`, `args[]`, `id` | `command` may be omitted when pinned |
| `list` | `since` (RFC3339), `id` | `since` filters by `last_event_at` |
| `stop` | `pid`, `id` | sends SIGTERM; SIGKILL after 5 s |
| `replay` | `pid`, `since` (RFC3339), `id` | replays ring buffer to caller only |

**Server → Client** (`type` field):

| type | fields |
|------|--------|
| `started` | `pid`, `ts`, `id` |
| `workers` | `workers[]` (WorkerInfo), `id` |
| `output` | `pid`, `stream` (stdout/stderr), `data`, `ts` |
| `exited` | `pid`, `exit_code`, `ts` |
| `error` | `message`, `id` |

The optional `id` field in client messages is echoed back in the response, allowing request/response correlation.

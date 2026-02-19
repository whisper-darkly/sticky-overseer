# sticky-overseer

WebSocket-based process overseer that spawns, tracks, and relays output from child processes in real-time.

## Build

```bash
make build
```

## Install

```bash
make install              # installs to /usr/local/bin
make install PREFIX=~/.local  # custom prefix
```

## Usage

```bash
# Basic
./sticky-overseer

# Pin to a specific command
./sticky-overseer -command sticky-recorder

# Custom port
OVERSEER_PORT=9090 ./sticky-overseer
```

Connect via WebSocket at `ws://host:8080/ws`.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `OVERSEER_PORT` | `8080` | Listen port |
| `OVERSEER_TRUSTED_CIDRS` | auto-detect | Comma-separated IPs/CIDRs allowed to connect |
| `OVERSEER_LOG_FILE` | — | Optional JSONL event log path |

## Protocol

All messages are JSON over WebSocket.

### Client Commands

- **start** — spawn a worker: `{"type":"start","id":"1","command":"echo","args":["hello"]}`
- **list** — list all workers: `{"type":"list","id":"2"}` (optional `since` filter)
- **stop** — SIGTERM a worker: `{"type":"stop","id":"3","pid":12345}`
- **replay** — replay buffered events: `{"type":"replay","id":"4","pid":12345}` (optional `since` filter)

### Server Events

- **started** — worker spawned with PID
- **output** — stdout/stderr line from a worker
- **exited** — worker exited with code
- **workers** — response to list
- **error** — error response

Workers retain their last 100 events in a ring buffer for replay after reconnection.

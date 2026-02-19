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
# Basic — any client may start any process
./sticky-overseer

# Pin to a specific command (clients may omit the command field or must match exactly)
./sticky-overseer -command /usr/local/bin/my-tool

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
| `OVERSEER_DB` | — | Optional SQLite path for persistent task IDs |

## Docker

A minimal Docker image (Alpine-based) is provided in `docker/`.

```bash
# Build
make -C docker build

# Build and export as .tar
make -C docker export

# Run
docker run -p 8080:8080 sticky-overseer:latest

# Pin to a command inside the container
docker run -p 8080:8080 sticky-overseer:latest sticky-overseer -command /usr/local/bin/my-tool
```

A `docker/compose.yaml` is also included as a starting point.

### Integrating with another tool

If you want to ship sticky-overseer bundled with your own binary (e.g. a recorder),
the recommended pattern is to build that image from **your** repo:

1. Start `FROM golang:1.24-alpine` and clone/build sticky-overseer alongside your own binary.
2. Set `CMD ["sticky-overseer", "-command", "/usr/local/bin/your-tool"]`.
3. Expose port 8080 (or whatever `OVERSEER_PORT` you configure).

Your image owns its runtime dependencies (e.g. ffmpeg) — sticky-overseer stays generic.

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

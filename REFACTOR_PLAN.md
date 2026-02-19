# sticky-overseer: Library Refactor Plan

This document is the **API contract** for converting sticky-overseer from a
`package main` binary into an importable Go library. A second agent (sticky-refinery)
codes against this contract before the refactor is complete. Do not change the
exported symbols described here without coordinating with that agent.

---

## Module / Package

| Item | Value |
|------|-------|
| Module path (`go.mod`) | `github.com/whisper-darkly/sticky-overseer` |
| Library package name | `overseer` |
| Library location | repo root (all non-cmd `.go` files) |
| Binary location | `cmd/sticky-overseer/main.go` |
| Import by consumers | `import overseer "github.com/whisper-darkly/sticky-overseer"` |

---

## Full Exported API

### Configuration

```go
// HubConfig holds all options for creating a Hub.
type HubConfig struct {
    DB            *sql.DB        // required; use OpenDB() to create
    PinnedCommand string         // optional; restricts start commands
    EventLog      *json.Encoder  // optional; JSONL event log writer
}
```

### Hub

```go
type Hub struct { /* opaque */ }

// NewHub creates a Hub, loads persisted tasks from DB, and marks them stopped.
func NewHub(cfg HubConfig) *Hub

// AddClient registers a WebSocket connection with the hub.
func (h *Hub) AddClient(conn *websocket.Conn)

// RemoveClient deregisters a WebSocket connection.
func (h *Hub) RemoveClient(conn *websocket.Conn)

// Broadcast serialises msg to JSON and writes it to every connected client.
func (h *Hub) Broadcast(msg any)

// HandleClient runs the read loop for conn until the connection closes.
// It calls AddClient on entry and RemoveClient+conn.Close on exit.
func (h *Hub) HandleClient(conn *websocket.Conn)
```

### HTTP handler

```go
// NewHandler returns an http.HandlerFunc that upgrades HTTP to WebSocket,
// enforces IP trust, and delegates to hub.HandleClient.
// Pass nil trustedNets to allow connections from any IP.
func NewHandler(h *Hub, trustedNets []*net.IPNet) http.HandlerFunc
```

### Database

```go
// OpenDB opens (or creates) the SQLite database at path and applies the schema.
// Pass ":memory:" for an in-memory database (useful in tests).
func OpenDB(path string) (*sql.DB, error)
```

### Network helpers

```go
// ParseTrustedCIDRs parses a comma-separated list of bare IPs and CIDR ranges.
// Returns nil, nil for an empty string; callers interpret nil as "allow all".
func ParseTrustedCIDRs(s string) ([]*net.IPNet, error)

// DetectLocalSubnets returns loopback (127.0.0.0/8, ::1/128) plus all
// subnets found on local network interfaces.
func DetectLocalSubnets() []*net.IPNet
```

`isTrusted` remains unexported — it is used only inside `NewHandler`.

### Types (unchanged, promoted to package `overseer`)

```go
// RetryPolicy controls automatic worker restarts.
type RetryPolicy struct {
    RestartDelay   string `json:"restart_delay,omitempty"`
    ErrorWindow    string `json:"error_window,omitempty"`
    ErrorThreshold int    `json:"error_threshold,omitempty"`
}

// TaskRecord is the persistent representation of a task (stored in SQLite).
type TaskRecord struct {
    TaskID        string
    Command       string
    Args          []string
    RetryPolicy   *RetryPolicy
    State         string // "active" | "stopped" | "errored"
    RestartCount  int
    ExitCount     int
    CreatedAt     time.Time
    LastStartedAt *time.Time
    LastExitedAt  *time.Time
    ErrorMessage  string
}

// WorkerConfig is passed to StartWorker.
type WorkerConfig struct {
    TaskID  string
    Command string
    Args    []string
}

// Worker wraps an exec.Cmd and streams output via the hub.
type Worker struct {
    PID       int
    TaskID    string
    Command   string
    Args      []string
    State     string // "running" | "exited"
    StartedAt time.Time
    ExitedAt  *time.Time
    ExitCode  *int
    // unexported fields omitted
}

// Event is stored in the per-worker ring buffer (last 100 events).
type Event struct {
    Type        string    `json:"type"`
    TaskID      string    `json:"task_id"`
    PID         int       `json:"pid"`
    Stream      string    `json:"stream,omitempty"`
    Data        string    `json:"data,omitempty"`
    ExitCode    *int      `json:"exit_code,omitempty"`
    Intentional bool      `json:"intentional,omitempty"`
    TS          time.Time `json:"ts"`
}

// Task is the in-memory representation of a persistent task.
type Task struct { /* opaque */ }
```

### WebSocket message types (unchanged)

```go
type IncomingMessage   struct { ... }  // client → server
type StartedMessage    struct { ... }  // server → client
type TaskInfo          struct { ... }  // embedded in TasksMessage
type TasksMessage      struct { ... }  // server → client (list response)
type OutputMessage     struct { ... }  // server → client
type ExitedMessage     struct { ... }  // server → client
type RestartingMessage struct { ... }  // server → client
type ErroredMessage    struct { ... }  // server → client
type ErrorMessage      struct { ... }  // server → client (error response)
```

All field names and JSON tags are identical to the current `package main` versions.

---

## What Moves Where

| Current file | Action | New location / package |
|---|---|---|
| `main.go` (root) | Delete | replaced by `cmd/sticky-overseer/main.go` |
| `hub.go` | `package main` → `package overseer`; `NewHub(*sql.DB)` → `NewHub(HubConfig)`; extract `NewHandler` | root, `package overseer` |
| `worker.go` | `package main` → `package overseer` | root, `package overseer` |
| `messages.go` | `package main` → `package overseer` | root, `package overseer` |
| `store.go` | `package main` → `package overseer`; `openDB` → `OpenDB` | root, `package overseer` |
| `hub_test.go` | `package main` → `package overseer`; update `NewHub` calls | root |
| `worker_test.go` | `package main` → `package overseer` | root |
| `store_test.go` | `package main` → `package overseer`; `openDB` → `OpenDB` | root |
| `main_test.go` | `package main` → `package overseer`; move `isTrusted`/`detectLocalSubnets` tests to use exported `ParseTrustedCIDRs`/`DetectLocalSubnets`; keep `newUUID`/`parseDuration` tests in root package | root, `package overseer` |
| `parseTrustedCIDRs()` in `main.go` | Export as `ParseTrustedCIDRs(s string) ([]*net.IPNet, error)` — signature changes: takes a string, returns error instead of calling `log.Fatal` | `hub.go` or new `net.go` |
| `detectLocalSubnets()` in `main.go` | Export as `DetectLocalSubnets() []*net.IPNet` | same file as above |
| `isTrusted()` in `main.go` | Stays unexported, moves into same file as above | same file |
| *(new)* `cmd/sticky-overseer/main.go` | Create | `cmd/sticky-overseer/`, `package main` |

---

## cmd/sticky-overseer/main.go (structure)

The binary wrapper is ~60 lines and preserves 100 % of the current CLI behaviour:

```go
package main

import (
    "encoding/json"
    "flag"
    "fmt"
    "log"
    "net/http"
    "os"

    overseer "github.com/whisper-darkly/sticky-overseer"
    _ "modernc.org/sqlite"
)

var (
    version = "dev"
    commit  = "unknown"
)

func main() {
    pinnedCmd   := flag.String("command", "", "Pin the allowed command")
    dbPath      := flag.String("db", "", "SQLite database path (default: $OVERSEER_DB or ./overseer.db)")
    showVersion := flag.Bool("version", false, "Print version and exit")
    showHelp    := flag.Bool("help", false, "Print usage and exit")
    flag.Parse()

    if *showHelp {
        fmt.Printf("sticky-overseer %s (%s)\n\n", version, commit)
        // ... env var help text identical to today ...
        os.Exit(0)
    }
    if *showVersion {
        fmt.Printf("sticky-overseer %s (%s)\n", version, commit)
        os.Exit(0)
    }

    port := os.Getenv("OVERSEER_PORT")
    if port == "" {
        port = "8080"
    }

    resolvedDB := *dbPath
    if resolvedDB == "" { resolvedDB = os.Getenv("OVERSEER_DB") }
    if resolvedDB == "" { resolvedDB = "./overseer.db" }

    db, err := overseer.OpenDB(resolvedDB)
    if err != nil {
        log.Fatalf("failed to open database %s: %v", resolvedDB, err)
    }
    defer db.Close()
    log.Printf("database: %s", resolvedDB)

    cfg := overseer.HubConfig{DB: db, PinnedCommand: *pinnedCmd}

    if logPath := os.Getenv("OVERSEER_LOG_FILE"); logPath != "" {
        f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
        if err != nil {
            log.Fatalf("failed to open log file %s: %v", logPath, err)
        }
        defer f.Close()
        cfg.EventLog = json.NewEncoder(f)
        log.Printf("event log: %s", logPath)
    }
    if *pinnedCmd != "" {
        log.Printf("command pinned to: %s", *pinnedCmd)
    }

    hub := overseer.NewHub(cfg)

    nets, err := overseer.ParseTrustedCIDRs(os.Getenv("OVERSEER_TRUSTED_CIDRS"))
    if err != nil {
        log.Fatalf("OVERSEER_TRUSTED_CIDRS: %v", err)
    }
    if nets == nil {
        nets = overseer.DetectLocalSubnets()
    }

    http.Handle("/ws", overseer.NewHandler(hub, nets))
    log.Printf("overseer listening on :%s", port)
    log.Fatal(http.ListenAndServe(":"+port, nil))
}
```

---

## Minimal embedder pattern (sticky-refinery)

```go
import (
    "net/http"
    overseer "github.com/whisper-darkly/sticky-overseer"
    _ "modernc.org/sqlite"
)

db, err := overseer.OpenDB("./tasks.db")
if err != nil { log.Fatal(err) }

hub := overseer.NewHub(overseer.HubConfig{
    DB:            db,
    PinnedCommand: "/usr/local/bin/my-tool",
})

nets := overseer.DetectLocalSubnets()
http.Handle("/ws", overseer.NewHandler(hub, nets))
log.Fatal(http.ListenAndServe(":8080", nil))
```

---

## Makefile change

```makefile
# Before
go build -o dist/sticky-overseer .

# After
go build -o dist/sticky-overseer ./cmd/sticky-overseer
```

---

## Verification checklist

```bash
go build ./...          # library + cmd both compile
go test ./...           # all tests pass
go vet ./...            # clean

./dist/sticky-overseer -help
./dist/sticky-overseer -version
./dist/sticky-overseer -command /bin/echo
# websocat ws://localhost:8080/ws → send start → receive "started"

make -C docker build    # Docker image still builds
```

Binary behaviour is **100 % identical**: same flags (`-command`, `-db`,
`-version`, `-help`), same env vars (`OVERSEER_PORT`, `OVERSEER_TRUSTED_CIDRS`,
`OVERSEER_LOG_FILE`, `OVERSEER_DB`), same `/ws` endpoint, same IP-allowlist
logic.

---

## Commit sequence

1. `docs: add REFACTOR_PLAN.md — library API contract`  ← this file only
2. `refactor: extract library package; binary moves to cmd/sticky-overseer`  ← all code changes

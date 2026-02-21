package overseer

import (
	_ "embed"

	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed docker/playground.html
var playgroundHTML []byte

//go:embed docs/swagger.json
var openAPISpec []byte

// Conn represents a single client connection regardless of the underlying transport.
// Implementations must be safe for concurrent ReadJSON calls from one goroutine
// while WriteJSON is protected via WriteLock.
type Conn interface {
	ReadJSON(v any) error
	WriteJSON(v any) error
	Close() error
	RemoteAddr() string
	WriteLock() *sync.Mutex
}

// Transport represents a network transport that produces Conn instances by
// listening for incoming connections. Callers receive connections via the
// channel returned by Listen.
type Transport interface {
	// Listen starts the transport and returns a channel of incoming connections.
	// The channel is closed when the context is cancelled or a fatal error occurs.
	Listen(ctx context.Context) (<-chan Conn, error)
	// String returns a human-readable identifier for this transport (e.g. ":8080").
	String() string
}

// ParseListenAddr converts a listen address string to the appropriate Transport:
//
//   - "stdio" (case-insensitive)  → stdioTransport{}
//   - strings starting with "/" or "./" → unixTransport{path}
//   - bare integer (e.g. "8080") → tcpTransport{addr: ":8080"}
//   - anything else              → tcpTransport{addr: addr}
func ParseListenAddr(addr string) (Transport, error) {
	// STDIO check (case-insensitive).
	if strings.EqualFold(addr, "stdio") {
		return stdioTransport{}, nil
	}

	// Unix socket: absolute path or relative starting with "./".
	if strings.HasPrefix(addr, "/") || strings.HasPrefix(addr, "./") {
		return unixTransport{path: addr}, nil
	}

	// Bare integer port → prefix with ":".
	if _, err := strconv.Atoi(addr); err == nil {
		return tcpTransport{addr: ":" + addr}, nil
	}

	// Everything else (e.g. "localhost:9090", ":8080") is used as-is.
	return tcpTransport{addr: addr}, nil
}

// ---------------------------------------------------------------------------
// wsConn — Conn implementation wrapping a gorilla *websocket.Conn
// ---------------------------------------------------------------------------

type wsConn struct {
	*websocket.Conn
	mu sync.Mutex
}

func (c *wsConn) ReadJSON(v any) error {
	return c.Conn.ReadJSON(v)
}

func (c *wsConn) WriteJSON(v any) error {
	return c.Conn.WriteJSON(v)
}

func (c *wsConn) Close() error {
	return c.Conn.Close()
}

func (c *wsConn) RemoteAddr() string {
	return c.Conn.RemoteAddr().String()
}

func (c *wsConn) WriteLock() *sync.Mutex {
	return &c.mu
}

// ---------------------------------------------------------------------------
// tcpTransport — HTTP/WebSocket transport
// ---------------------------------------------------------------------------

// tcpTransport serves an HTTP server that upgrades connections to WebSocket on
// "/ws" and serves the playground UI at "/".
type tcpTransport struct {
	addr        string
	trustedNets []*net.IPNet
}

// TrustedNets is a convenience setter used by tests and main.go wiring.
// It is not part of the Transport interface.
func (t tcpTransport) withTrustedNets(nets []*net.IPNet) tcpTransport {
	t.trustedNets = nets
	return t
}

func (t tcpTransport) String() string {
	return t.addr
}

func (t tcpTransport) Listen(ctx context.Context) (<-chan Conn, error) {
	ch := make(chan Conn, 1)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	mux := http.NewServeMux()

	// Serve playground at "/".
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(playgroundHTML)
	})

	// Serve OpenAPI spec.
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openAPISpec)
	})

	// WebSocket endpoint.
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !isTrusted(r, t.trustedNets) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rawConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("tcp transport: upgrade error: %v", err)
			return
		}
		conn := &wsConn{Conn: rawConn}
		select {
		case ch <- conn:
		case <-ctx.Done():
			rawConn.Close()
		}
	})

	srv := &http.Server{
		Addr:    t.addr,
		Handler: mux,
	}

	// Start listening before launching the goroutine so callers know the port
	// is bound by the time Listen returns (important for ":0" dynamic ports).
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return nil, fmt.Errorf("tcp transport: listen %s: %w", t.addr, err)
	}

	go func() {
		defer close(ch)
		defer ln.Close()

		// Serve in the background; shut it down when ctx is cancelled.
		srvErr := make(chan error, 1)
		go func() {
			srvErr <- srv.Serve(ln)
		}()

		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			srv.Shutdown(shutdownCtx) //nolint:errcheck
		case err := <-srvErr:
			if err != nil && err != http.ErrServerClosed {
				log.Printf("tcp transport: server error: %v", err)
			}
		}
	}()

	return ch, nil
}

// Addr returns the actual listener address, resolving ":0" to the assigned port.
// This is a helper used by tests and is not part of the Transport interface.
func tcpListenerAddr(t tcpTransport) string {
	return t.addr
}

// ---------------------------------------------------------------------------
// stdioConn — Conn implementation wrapping stdin/stdout
// ---------------------------------------------------------------------------

type stdioConn struct {
	scanner *bufio.Scanner
	enc     *json.Encoder
	mu      sync.Mutex
	once    sync.Once
	closed  chan struct{}
	r       io.Reader // retained for Close to drain if needed
	w       io.Writer
}

func newStdioConn(r io.Reader, w io.Writer) *stdioConn {
	c := &stdioConn{
		scanner: bufio.NewScanner(r),
		enc:     json.NewEncoder(w),
		closed:  make(chan struct{}),
		r:       r,
		w:       w,
	}
	return c
}

func (c *stdioConn) ReadJSON(v any) error {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	return json.Unmarshal(c.scanner.Bytes(), v)
}

func (c *stdioConn) WriteJSON(v any) error {
	return c.enc.Encode(v)
}

func (c *stdioConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *stdioConn) RemoteAddr() string {
	return "stdio"
}

func (c *stdioConn) WriteLock() *sync.Mutex {
	return &c.mu
}

// ---------------------------------------------------------------------------
// stdioTransport — single-connection transport over stdin/stdout
// ---------------------------------------------------------------------------

// stdioTransport emits exactly one Conn (wrapping os.Stdin/os.Stdout) and then
// closes the channel. It is primarily useful for integration testing and
// embedding in scripts.
type stdioTransport struct{}

func (t stdioTransport) String() string {
	return "STDIO"
}

func (t stdioTransport) Listen(ctx context.Context) (<-chan Conn, error) {
	return t.listenOn(ctx, os.Stdin, os.Stdout)
}

// listenOn is the testable core of Listen; it accepts arbitrary io.Reader/Writer.
func (t stdioTransport) listenOn(ctx context.Context, r io.Reader, w io.Writer) (<-chan Conn, error) {
	ch := make(chan Conn, 1)
	conn := newStdioConn(r, w)

	go func() {
		defer close(ch)
		select {
		case ch <- conn:
		case <-ctx.Done():
			conn.Close()
		}
	}()

	return ch, nil
}

// ---------------------------------------------------------------------------
// unixTransport — WebSocket-over-Unix-domain-socket transport
// ---------------------------------------------------------------------------

// unixTransport accepts connections on a Unix domain socket and upgrades each
// one to WebSocket, producing wsConn instances identical to tcpTransport.
type unixTransport struct {
	path        string
	trustedNets []*net.IPNet // optional; usually nil for local socket
}

func (t unixTransport) String() string {
	return t.path
}

func (t unixTransport) Listen(ctx context.Context) (<-chan Conn, error) {
	// Remove stale socket file if present.
	_ = os.Remove(t.path)

	ln, err := net.Listen("unix", t.path)
	if err != nil {
		return nil, fmt.Errorf("unix transport: listen %s: %w", t.path, err)
	}

	ch := make(chan Conn, 1)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if len(t.trustedNets) > 0 && !isTrusted(r, t.trustedNets) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rawConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("unix transport: upgrade error: %v", err)
			return
		}
		conn := &wsConn{Conn: rawConn}
		select {
		case ch <- conn:
		case <-ctx.Done():
			rawConn.Close()
		}
	})

	srv := &http.Server{Handler: mux}

	go func() {
		defer close(ch)
		defer os.Remove(t.path)
		defer ln.Close()

		srvErr := make(chan error, 1)
		go func() {
			srvErr <- srv.Serve(ln)
		}()

		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			srv.Shutdown(shutdownCtx) //nolint:errcheck
		case err := <-srvErr:
			if err != nil && err != http.ErrServerClosed {
				log.Printf("unix transport: server error: %v", err)
			}
		}
	}()

	return ch, nil
}

// @title           sticky-overseer API
// @version         1.1
// @description     WebSocket-based process overseer for spawning and tracking child processes
// @host            localhost:8080
// @BasePath        /

//go:generate swag init --output ./docs

package overseer

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// RunCLI runs the full sticky-overseer CLI. Call from cmd/sticky-overseer/main.go.
// version and commit are injected via ldflags.
func RunCLI(version, commit string) {
	configPath := flag.String("config", "./config.yaml", "Path to YAML configuration file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	showHelp := flag.Bool("help", false, "Print usage and exit")
	listHandlers := flag.Bool("list-handlers", false, "Print registered action handler types and exit")
	flag.Parse()

	// Apply OVERSEER_CONFIG env override to config path before anything else.
	if envCfg := os.Getenv("OVERSEER_CONFIG"); envCfg != "" {
		*configPath = envCfg
	}

	if *showHelp {
		fmt.Fprintf(os.Stderr, "sticky-overseer %s (%s)\n\n", version, commit)
		fmt.Fprintln(os.Stderr, "WebSocket-based process overseer for spawning and tracking child processes.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Environment:")
		fmt.Fprintln(os.Stderr, "  OVERSEER_CONFIG         Path to YAML config file (overrides -config flag)")
		fmt.Fprintln(os.Stderr, "  OVERSEER_LISTEN         Listen address (overrides config listen)")
		fmt.Fprintln(os.Stderr, "  OVERSEER_DB             SQLite database path (overrides config db)")
		fmt.Fprintln(os.Stderr, "  OVERSEER_LOG_FILE       Optional JSONL event log path")
		os.Exit(0)
	}

	if *showVersion {
		fmt.Printf("sticky-overseer %s (%s)\n", version, commit)
		os.Exit(0)
	}

	if *listHandlers {
		ListHandlers()
		os.Exit(0)
	}

	// 1. Load configuration.
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// 2. Apply ENV overrides after config load (these are fixed for the process lifetime).
	if listenEnv := os.Getenv("OVERSEER_LISTEN"); listenEnv != "" {
		cfg.Listen = listenEnv
	}
	if dbEnv := os.Getenv("OVERSEER_DB"); dbEnv != "" {
		cfg.DB = dbEnv
	}

	// 3. Open database (includes v2→v3 migration).
	db, err := OpenDB(cfg.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database %s: %v\n", cfg.DB, err)
		os.Exit(1)
	}
	defer db.Close()
	log.Printf("database: %s", cfg.DB)

	// 4. Build action handlers from config.
	actions, err := BuildActionHandlers(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build action handlers: %v\n", err)
		os.Exit(1)
	}
	log.Printf("registered %d action(s)", len(actions))

	// 5. Initialize pool manager.
	poolCfgs := extractPoolConfigs(cfg)
	pool := NewPoolManager(cfg.TaskPool, poolCfgs)

	// 6. Parse listen address and select transport.
	transport, err := ParseListenAddr(cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse listen address %q: %v\n", cfg.Listen, err)
		os.Exit(1)
	}

	// 7. Build trusted networks list.
	trustedNets, err := buildTrustedNets(cfg.TrustedCIDRs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse trusted CIDRs: %v\n", err)
		os.Exit(1)
	}

	// Attach trusted nets to TCP transport if applicable.
	if tcp, ok := transport.(tcpTransport); ok {
		transport = tcp.withTrustedNets(trustedNets)
	}

	// 8. Open event log if configured (OVERSEER_LOG_FILE env or cfg.LogFile).
	logPath := cfg.LogFile
	if envLog := os.Getenv("OVERSEER_LOG_FILE"); envLog != "" {
		logPath = envLog
	}
	var eventLog io.Writer
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open log file %s: %v\n", logPath, err)
			os.Exit(1)
		}
		defer f.Close()
		eventLog = f
		log.Printf("event log: %s", logPath)
	}

	// 9. Create Hub.
	hub := NewHub(HubConfig{
		DB:          db,
		EventLog:    eventLog,
		Actions:     actions,
		Pool:        pool,
		TrustedNets: trustedNets,
	})

	// 10. Setup graceful shutdown context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		cancel()
	}()

	// 11. Start transport listening.
	connChan, err := transport.Listen(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start transport on %s: %v\n", transport.String(), err)
		os.Exit(1)
	}
	log.Printf("overseer listening on %s", transport.String())

	// 12. Accept connections until context is cancelled (connChan closes).
	for conn := range connChan {
		go hub.HandleClient(conn)
	}

	// 13. Graceful shutdown — pool drain then worker shutdown.
	pool.DrainAll()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := hub.Shutdown(shutdownCtx); err != nil {
		log.Printf("hub shutdown: %v", err)
	}

	// DB is closed by deferred db.Close() above.
	log.Println("shutdown complete")
}

// extractPoolConfigs builds a map of per-action PoolConfig values from the
// global Config, keyed by action name. Used to initialise the PoolManager.
func extractPoolConfigs(cfg *Config) map[string]PoolConfig {
	pcs := make(map[string]PoolConfig, len(cfg.Actions))
	for name, ac := range cfg.Actions {
		pcs[name] = ac.TaskPool
	}
	return pcs
}

// buildTrustedNets returns the trusted networks to use for IP allowlisting.
// If cfg.TrustedCIDRs is non-empty those are used; otherwise the local
// interfaces and loopback addresses are auto-detected.
func buildTrustedNets(cidrList []string) ([]*net.IPNet, error) {
	if len(cidrList) > 0 {
		var nets []*net.IPNet
		for _, cidr := range cidrList {
			_, ipnet, err := net.ParseCIDR(cidr)
			if err != nil {
				// Try bare IP (no prefix length).
				ip := net.ParseIP(cidr)
				if ip == nil {
					return nil, fmt.Errorf("invalid CIDR or IP %q: %w", cidr, err)
				}
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				_, ipnet, _ = net.ParseCIDR(fmt.Sprintf("%s/%d", cidr, bits))
			}
			nets = append(nets, ipnet)
		}
		return nets, nil
	}
	return detectLocalSubnets(), nil
}

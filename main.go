package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed docker/playground.html
var playgroundHTML []byte

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	pinnedCmd   := flag.String("command", "", "Pin the allowed command (clients cannot override)")
	dbPath      := flag.String("db", "", "SQLite database path (default: $OVERSEER_DB or ./overseer.db)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	showHelp    := flag.Bool("help", false, "Print usage and exit")
	flag.Parse()

	if *showHelp {
		fmt.Printf("sticky-overseer %s (%s)\n\n", version, commit)
		fmt.Println("WebSocket-based process overseer for spawning and tracking child processes.")
		fmt.Println()
		fmt.Println("Flags:")
		flag.PrintDefaults()
		fmt.Println()
		fmt.Println("Environment:")
		fmt.Println("  OVERSEER_PORT           Listen port (default: 8080)")
		fmt.Println("  OVERSEER_TRUSTED_CIDRS  Comma-separated IPs/CIDRs (default: auto-detect local)")
		fmt.Println("  OVERSEER_LOG_FILE       Optional JSONL event log path")
		fmt.Println("  OVERSEER_DB             SQLite database path (default: ./overseer.db)")
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
	if resolvedDB == "" {
		resolvedDB = os.Getenv("OVERSEER_DB")
	}
	if resolvedDB == "" {
		resolvedDB = "./overseer.db"
	}

	db, err := openDB(resolvedDB)
	if err != nil {
		log.Fatalf("failed to open database %s: %v", resolvedDB, err)
	}
	defer db.Close()
	log.Printf("database: %s", resolvedDB)

	cfg := hubConfig{
		DB:            db,
		PinnedCommand: *pinnedCmd,
	}

	if logPath := os.Getenv("OVERSEER_LOG_FILE"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("failed to open log file %s: %v", logPath, err)
		}
		defer f.Close()
		cfg.EventLog = f
		log.Printf("event log: %s", logPath)
	}

	if *pinnedCmd != "" {
		log.Printf("command pinned to: %s", *pinnedCmd)
	}

	hub := newHub(cfg)

	nets, err := parseTrustedCIDRs(os.Getenv("OVERSEER_TRUSTED_CIDRS"))
	if err != nil {
		log.Fatalf("OVERSEER_TRUSTED_CIDRS: %v", err)
	}
	if nets == nil {
		nets = detectLocalSubnets()
	}

	http.Handle("/ws", newHandler(hub, nets))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(playgroundHTML)
	})

	srv := &http.Server{Addr: ":" + port}

	go func() {
		log.Printf("overseer listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received %v, shutting down", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Printf("hub shutdown: %v", err)
	}
	log.Println("shutdown complete")
}

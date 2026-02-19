package main

import (
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

	db, err := overseer.OpenDB(resolvedDB)
	if err != nil {
		log.Fatalf("failed to open database %s: %v", resolvedDB, err)
	}
	defer db.Close()
	log.Printf("database: %s", resolvedDB)

	cfg := overseer.HubConfig{
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

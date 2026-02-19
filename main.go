package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

var (
	version = "dev"
	commit  = "unknown"

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func main() {
	pinnedCmd := flag.String("command", "", "Pin the allowed command (clients cannot override)")
	dbPath := flag.String("db", "", "SQLite database path (default: $OVERSEER_DB or ./overseer.db)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	showHelp := flag.Bool("help", false, "Print usage and exit")
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

	trustedNets := parseTrustedCIDRs()

	resolvedDBPath := *dbPath
	if resolvedDBPath == "" {
		resolvedDBPath = os.Getenv("OVERSEER_DB")
	}
	if resolvedDBPath == "" {
		resolvedDBPath = "./overseer.db"
	}
	db, err := openDB(resolvedDBPath)
	if err != nil {
		log.Fatalf("failed to open database %s: %v", resolvedDBPath, err)
	}
	defer db.Close()
	log.Printf("database: %s", resolvedDBPath)

	hub := NewHub(db)

	if logPath := os.Getenv("OVERSEER_LOG_FILE"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("failed to open log file %s: %v", logPath, err)
		}
		defer f.Close()
		hub.eventLog = json.NewEncoder(f)
		log.Printf("event log: %s", logPath)
	}
	hub.pinnedCommand = *pinnedCmd
	if *pinnedCmd != "" {
		log.Printf("command pinned to: %s", *pinnedCmd)
	}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !isTrusted(r, trustedNets) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}
		log.Printf("client connected from %s", r.RemoteAddr)
		hub.HandleClient(conn)
		log.Printf("client disconnected from %s", r.RemoteAddr)
	})

	addr := ":" + port
	log.Printf("overseer listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func parseTrustedCIDRs() []*net.IPNet {
	raw := os.Getenv("OVERSEER_TRUSTED_CIDRS")
	if raw != "" {
		var nets []*net.IPNet
		for _, cidr := range strings.Split(raw, ",") {
			cidr = strings.TrimSpace(cidr)
			if !strings.Contains(cidr, "/") {
				// Bare IP — treat as /32 or /128
				ip := net.ParseIP(cidr)
				if ip == nil {
					log.Fatalf("invalid IP in OVERSEER_TRUSTED_CIDRS: %s", cidr)
				}
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				_, ipnet, _ := net.ParseCIDR(fmt.Sprintf("%s/%d", cidr, bits))
				nets = append(nets, ipnet)
			} else {
				_, ipnet, err := net.ParseCIDR(cidr)
				if err != nil {
					log.Fatalf("invalid CIDR in OVERSEER_TRUSTED_CIDRS: %s", cidr)
				}
				nets = append(nets, ipnet)
			}
		}
		return nets
	}

	// Auto-detect local subnets
	return detectLocalSubnets()
}

func detectLocalSubnets() []*net.IPNet {
	var nets []*net.IPNet
	// Always trust loopback
	_, lo4, _ := net.ParseCIDR("127.0.0.0/8")
	_, lo6, _ := net.ParseCIDR("::1/128")
	nets = append(nets, lo4, lo6)

	ifaces, err := net.Interfaces()
	if err != nil {
		return nets
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				nets = append(nets, ipnet)
			}
		}
	}
	return nets
}

func isTrusted(r *http.Request, nets []*net.IPNet) bool {
	if len(nets) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

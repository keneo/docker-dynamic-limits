package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/keneo/docker-dynamic-limits/internal/api"
	"github.com/keneo/docker-dynamic-limits/internal/cgroup"
	"github.com/keneo/docker-dynamic-limits/internal/docker"
	"github.com/keneo/docker-dynamic-limits/internal/enforcement"
	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/proxy"
	"github.com/keneo/docker-dynamic-limits/internal/store"
)

func main() {
	addr := flag.String("addr", ":7123", "read-only TCP listen address for containers")
	sock := flag.String("sock", "/run/ddl/ddl.sock", "management API unix socket path (empty to disable)")
	dbPath := flag.String("db", "/var/lib/ddl/ddl.db", "SQLite database path")
	cgroupBase := flag.String("cgroup-base", "/sys/fs/cgroup", "cgroup v2 base path")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("ddld starting...")

	// Ensure data directory exists
	if dir := dirOf(*dbPath); dir != "" {
		os.MkdirAll(dir, 0755)
	}

	// Initialize store
	st, err := store.New(*dbPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	// Initialize Docker client
	dc, err := docker.NewClient()
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}
	defer dc.Close()

	// Initialize cgroup reader
	cg := cgroup.NewReader(*cgroupBase)

	// Initialize spending proxy
	px := proxy.NewSpendingTracker(func(containerID string, totalMilliCents int64) {
		st.SetUsage(containerID, model.LimitSpending, totalMilliCents)
	})

	// Configure API keys for HTTP→HTTPS relay
	apiKeys := make(map[string]string)
	if key := os.Getenv("DDL_ANTHROPIC_API_KEY"); key != "" {
		apiKeys["api.anthropic.com"] = key
		log.Println("Anthropic API key configured for HTTP relay")
	}
	if key := os.Getenv("DDL_OPENAI_API_KEY"); key != "" {
		apiKeys["api.openai.com"] = key
		log.Println("OpenAI API key configured for HTTP relay")
	}
	if len(apiKeys) > 0 {
		px.SetAPIKeys(apiKeys)
	}

	// Apply proxy resolve overrides (for testing with mock API servers)
	// Format: DDL_PROXY_RESOLVE=api.openai.com=127.0.0.1,api.anthropic.com=127.0.0.1
	if resolves := os.Getenv("DDL_PROXY_RESOLVE"); resolves != "" {
		overrides := make(map[string]string)
		for _, entry := range strings.Split(resolves, ",") {
			parts := strings.SplitN(entry, "=", 2)
			if len(parts) == 2 {
				overrides[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
		px.SetResolveOverrides(overrides)
		log.Printf("proxy resolve overrides: %v", overrides)
	}

	// Initialize event bus
	bus := events.NewBus()

	// Initialize enforcement manager
	em := enforcement.NewManager(st, dc, cg, px, bus)

	// Start enforcement for all existing containers
	if err := em.StartAll(); err != nil {
		log.Printf("warning: failed to start enforcement for existing containers: %v", err)
	}

	// Create API server
	srv := api.NewServer(st, dc, em, px, bus)

	// Try to start the unix socket server for full management API.
	// If it fails (e.g. path not writable), fall back to full API on TCP.
	var sockServer *http.Server
	sockOK := false
	if *sock != "" {
		sockDir := filepath.Dir(*sock)
		os.MkdirAll(sockDir, 0755)
		os.Remove(*sock)

		listener, err := net.Listen("unix", *sock)
		if err != nil {
			log.Printf("warning: unix socket unavailable (%v), serving full API on TCP", err)
		} else {
			sockOK = true
			sockServer = &http.Server{Handler: srv.Handler()}
			go func() {
				log.Printf("management API listening on unix:%s", *sock)
				if err := sockServer.Serve(listener); err != http.ErrServerClosed {
					log.Fatalf("unix socket server error: %v", err)
				}
			}()
		}
	}

	// Start TCP server.
	// Read-only when the unix socket is active; full API otherwise.
	var tcpHandler http.Handler
	if sockOK {
		tcpHandler = srv.ReadOnlyHandler()
		log.Printf("read-only TCP API listening on %s", *addr)
	} else {
		tcpHandler = srv.Handler()
		log.Printf("full TCP API listening on %s", *addr)
	}
	tcpServer := &http.Server{
		Addr:    *addr,
		Handler: tcpHandler,
	}

	go func() {
		if err := tcpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("TCP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Printf("received signal %v, shutting down...", sig)
	srv.Stop()
	em.StopAll()
	tcpServer.Close()
	if sockServer != nil {
		sockServer.Close()
	}
	if *sock != "" {
		os.Remove(*sock)
	}
	log.Println("ddld stopped")
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}


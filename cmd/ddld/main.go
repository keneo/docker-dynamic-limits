package main

import (
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/api"
	"github.com/keneo/docker-dynamic-limits/internal/cgroup"
	"github.com/keneo/docker-dynamic-limits/internal/docker"
	"github.com/keneo/docker-dynamic-limits/internal/enforcement"
	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/ollama"
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

	// Tee logs to a file alongside the database
	if dir := dirOf(*dbPath); dir != "" {
		logFile, err := os.OpenFile(filepath.Join(dir, "ddld.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		}
	}

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

	// Provider enable/disable flags
	enabledHosts := map[string]bool{}
	if envBoolOr("DDL_ENABLE_OPENAI", os.Getenv("DDL_OPENAI_API_KEY") != "") {
		enabledHosts["api.openai.com"] = true
	}
	if envBoolOr("DDL_ENABLE_ANTHROPIC", os.Getenv("DDL_ANTHROPIC_API_KEY") != "") {
		enabledHosts["api.anthropic.com"] = true
	}
	px.SetEnabledHosts(enabledHosts)

	// Initialize event bus
	bus := events.NewBus()

	// Initialize enforcement manager
	em := enforcement.NewManager(st, dc, cg, px, bus)

	// Configure shared proxy address (containers reach proxy via daemon container name)
	sharedProxyPort := "7180"
	if p := os.Getenv("DDL_SHARED_PROXY_PORT"); p != "" {
		sharedProxyPort = p
	}
	// Containers address the proxy via the daemon container name on the Docker network.
	// The daemon container is named "ddl-daemon" by the CLI.
	sharedAddr := "ddl-daemon:" + sharedProxyPort
	if a := os.Getenv("DDL_SHARED_PROXY_ADDR"); a != "" {
		sharedAddr = a
	}
	px.SetSharedProxyAddr(sharedAddr)

	// Restore proxy tracking for existing containers.
	if containers, err := st.ListContainers(); err == nil {
		for _, c := range containers {
			budget, _ := st.GetLimit(c.ID, model.LimitSpending)
			spending, _ := st.GetUsage(c.ID, model.LimitSpending)
			addr, err := px.RegisterContainer(c.ID, budget, spending)
			if err != nil {
				log.Printf("warning: failed to restore proxy for %s: %v", c.ID, err)
			} else {
				log.Printf("[proxy] restored proxy for container %s on %s (spending: %d, budget: %d)", c.ID, addr, spending, budget)
			}
		}
	}

	// Start enforcement for all existing containers
	if err := em.StartAll(); err != nil {
		log.Printf("warning: failed to start enforcement for existing containers: %v", err)
	}

	// Ollama queue
	var oq *ollama.Queue
	ollamaURL := os.Getenv("DDL_OLLAMA_URL")
	if ollamaURL != "" && envBoolOr("DDL_ENABLE_OLLAMA", true) {
		cfg := ollama.Config{
			OllamaURL:      ollamaURL,
			AllowedModels:  parseCSV(os.Getenv("DDL_OLLAMA_MODELS")),
			MaxQueueSize:   parseIntOr(os.Getenv("DDL_OLLAMA_MAX_QUEUE"), 50),
			RequestTimeout: parseDurationOr(os.Getenv("DDL_OLLAMA_TIMEOUT"), 120*time.Second),
			DefaultBid:     int64(parseIntOr(os.Getenv("DDL_OLLAMA_DEFAULT_BID"), 0)),
		}
		oq = ollama.NewQueue(cfg, px, st, bus)
		oq.SetActivityRecorder(func(containerID string, act proxy.ProxyActivity) {
			px.RecordActivity(containerID, act)
		})
		px.SetOllamaHandler(oq)
		px.EnableHost("ollama", true)
		defer oq.Stop()
		log.Printf("Ollama proxy configured: %s (models: %v)", ollamaURL, cfg.AllowedModels)

		// Restore persisted bids for existing containers
		if containers, err := st.ListContainers(); err == nil {
			ids := make([]string, len(containers))
			for i, c := range containers {
				ids[i] = c.ID
			}
			oq.RestoreBids(ids)
		}
	}

	// Start scope enforcement (host + all segments)
	em.StartScopeEnforcement()

	// Create API server with config persistence
	configPath := filepath.Join(filepath.Dir(*dbPath), "config.json")
	srv := api.NewServer(st, dc, em, px, bus, oq, configPath)

	// Load persisted config (overlay on top of env var defaults)
	srv.LoadPersistedConfig()

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

	// Start shared proxy listener (containers identified by source IP)
	proxyAddr := ":" + sharedProxyPort
	px.SetIPResolver(func(ip string) string {
		return srv.ResolveContainerByIP(ip)
	})
	sharedProxyServer := &http.Server{
		Addr:    proxyAddr,
		Handler: px.SharedProxyHandler(),
	}
	go func() {
		log.Printf("shared proxy listening on %s (containers identified by source IP)", proxyAddr)
		if err := sharedProxyServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("warning: shared proxy error: %v", err)
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

func envBoolOr(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	switch strings.ToLower(v) {
	case "false", "0", "no":
		return false
	case "true", "1", "yes":
		return true
	default:
		return defaultVal
	}
}

func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func parseIntOr(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func parseDurationOr(s string, defaultVal time.Duration) time.Duration {
	if s == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultVal
	}
	return d
}


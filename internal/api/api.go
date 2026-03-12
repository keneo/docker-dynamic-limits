package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/docker"
	"github.com/keneo/docker-dynamic-limits/internal/enforcement"
	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/ollama"
	"github.com/keneo/docker-dynamic-limits/internal/proxy"
	"github.com/keneo/docker-dynamic-limits/internal/store"
)

// Server is the REST API server for ddld.
type Server struct {
	store       store.DataStore
	docker      docker.DockerClient
	enforcement enforcement.EnforcementController
	proxy       proxy.SpendingProxy
	bus         *events.Bus
	ollama      *ollama.Queue
	mux         *http.ServeMux

	ipMu  sync.RWMutex
	ipMap map[string]string // IP → containerID
	done  chan struct{}
}

// NewServer creates a new API server. oq may be nil when Ollama is not configured.
func NewServer(st store.DataStore, dc docker.DockerClient, em enforcement.EnforcementController, px proxy.SpendingProxy, bus *events.Bus, oq *ollama.Queue) *Server {
	s := &Server{
		store:       st,
		docker:      dc,
		enforcement: em,
		proxy:       px,
		bus:         bus,
		ollama:      oq,
		mux:         http.NewServeMux(),
		ipMap:       make(map[string]string),
		done:        make(chan struct{}),
	}
	s.registerRoutes()
	s.refreshIPs()
	go s.ipRefreshLoop()
	return s
}

// Stop shuts down the background IP refresh goroutine.
func (s *Server) Stop() {
	close(s.done)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/containers", s.handleContainers)
	s.mux.HandleFunc("/containers/", s.handleContainer)
	s.mux.HandleFunc("/register", s.handleRegister)
	s.mux.HandleFunc("/events", s.handleEvents)
	// In-container query endpoints (container identifies itself by source IP or token)
	s.mux.HandleFunc("/usage", s.handleSelfUsage)
	s.mux.HandleFunc("/limits", s.handleSelfLimits)
	// Ollama queue endpoints (full API)
	s.mux.HandleFunc("/ollama/queue", s.handleOllamaQueue)
	s.mux.HandleFunc("/ollama/models", s.handleOllamaModels)
	// Provider management
	s.mux.HandleFunc("/providers", s.handleProviders)
	// Runtime config management
	s.mux.HandleFunc("/config", s.handleConfig)
}

// Handler returns the HTTP handler (full API).
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ReadOnlyHandler returns an HTTP handler with only read-only, guest-facing routes.
// This is intended for the TCP listener that containers can reach.
// Container identification is done via source IP resolution.
func (s *Server) ReadOnlyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers", s.handleContainersReadOnly)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/usage", s.handleSelfUsageByIP)
	mux.HandleFunc("/limits", s.handleSelfLimitsByIP)
	// Ollama container-facing endpoints (identified by source IP)
	mux.HandleFunc("/ollama/queue", s.handleOllamaQueue)
	mux.HandleFunc("/ollama/models", s.handleOllamaModels)
	mux.HandleFunc("/ollama/bid", s.handleOllamaBidByIP)
	return mux
}

func (s *Server) handleContainersReadOnly(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.handleContainers(w, r)
}

// --- Container management ---

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	containers, err := s.store.ListContainers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var result []model.ContainerStatus
	for _, c := range containers {
		status := s.buildStatus(c)
		result = append(result, status)
	}
	writeJSON(w, result)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ContainerID string `json:"container_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ContainerID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("container_id required"))
		return
	}

	// Inspect the Docker container to validate it exists
	ctx := context.Background()
	info, err := s.docker.InspectContainer(ctx, req.ContainerID)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("container not found: %w", err))
		return
	}

	name := strings.TrimPrefix(info.Name, "/")

	c, err := s.store.RegisterContainer(info.ID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Start enforcement
	s.enforcement.StartContainer(c.ID, c.DockerID)

	if s.bus != nil {
		s.bus.PublishData(events.ContainerRegister, c.ID, events.ContainerRegisterData{
			DockerID: c.DockerID,
			Name:     name,
		})
	}

	// Set up spending proxy if needed
	var proxyAddr string
	if s.proxy != nil {
		var err error
		proxyAddr, err = s.proxy.RegisterContainer(c.ID, 0, 0)
		if err != nil {
			log.Printf("[api] warning: failed to start proxy for %s: %v", c.ID, err)
		} else {
			log.Printf("[api] proxy for %s available at %s", c.ID, proxyAddr)
		}
	}

	writeJSON(w, map[string]interface{}{
		"id":               c.ID,
		"docker_id":        c.DockerID,
		"name":             c.Name,
		"registered_at":    c.RegisteredAt,
		"proxy_addr":       proxyAddr,
		"ollama_available": s.ollama != nil,
	})
}

func (s *Server) handleContainer(w http.ResponseWriter, r *http.Request) {
	// Parse: /containers/{id}, /containers/{id}/limits, /containers/{id}/usage, /containers/{id}/clone
	path := strings.TrimPrefix(r.URL.Path, "/containers/")
	parts := strings.SplitN(path, "/", 2)
	containerQuery := parts[0]

	containerID, err := s.store.ResolveContainerID(containerQuery)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	if len(parts) == 1 {
		s.handleContainerInfo(w, r, containerID)
		return
	}

	sub := parts[1]
	switch {
	case sub == "limits":
		s.handleLimits(w, r, containerID)
	case sub == "usage":
		s.handleUsage(w, r, containerID)
	case sub == "clone":
		s.handleClone(w, r, containerID)
	case sub == "ollama/bid":
		s.handleOllamaBidForContainer(w, r, containerID)
	case sub == "ollama/queue":
		s.handleOllamaCancelForContainer(w, r, containerID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleContainerInfo(w http.ResponseWriter, r *http.Request, containerID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Method == http.MethodDelete {
		s.enforcement.StopContainer(containerID)
		if s.ollama != nil {
			s.ollama.RemoveContainer(containerID)
		}
		if err := s.store.RemoveContainer(containerID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.bus != nil {
			s.bus.PublishData(events.ContainerRemove, containerID, events.ContainerRemoveData{})
		}
		writeJSON(w, map[string]string{"status": "removed"})
		return
	}

	c, err := s.store.GetContainer(containerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	status := s.buildStatus(*c)
	writeJSON(w, status)
}

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request, containerID string) {
	switch r.Method {
	case http.MethodGet:
		limits, err := s.store.GetAllLimits(containerID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, limits)

	case http.MethodPut:
		var req struct {
			Type      string `json:"type"`
			Value     int64  `json:"value"`
			Operation string `json:"operation"` // "set", "increase", "decrease"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		lt := model.LimitType(req.Type)
		oldValue, _ := s.store.GetLimit(containerID, lt)
		var newValue int64

		switch req.Operation {
		case "set", "":
			newValue = req.Value
		case "increase":
			newValue = oldValue + req.Value
		case "decrease":
			newValue = oldValue - req.Value
			if newValue < 0 {
				newValue = 0
			}
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid operation: %s", req.Operation))
			return
		}

		if err := s.store.SetLimit(containerID, lt, newValue); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		// Apply RAM limit immediately via Docker
		if lt == model.LimitRAM {
			c, err := s.store.GetContainer(containerID)
			if err == nil {
				ctx := context.Background()
				s.docker.UpdateMemoryLimit(ctx, c.DockerID, newValue)
			}
		}

		// Update spending budget in proxy
		if lt == model.LimitSpending && s.proxy != nil {
			s.proxy.UpdateBudget(containerID, newValue)
		}

		s.enforcement.NotifyLimitChanged(containerID)

		if s.bus != nil {
			op := req.Operation
			if op == "" {
				op = "set"
			}
			s.bus.PublishData(events.LimitChange, containerID, events.LimitChangeData{
				LimitType: req.Type,
				OldValue:  oldValue,
				NewValue:  newValue,
				Operation: op,
			})
		}

		writeJSON(w, map[string]interface{}{
			"type":  req.Type,
			"value": newValue,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request, containerID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	usage, err := s.store.GetAllUsage(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, usage)
}

func (s *Server) handleClone(w http.ResponseWriter, r *http.Request, containerID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	c, err := s.store.GetContainer(containerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	ctx := context.Background()
	newDockerID, err := s.docker.CloneContainer(ctx, c.DockerID, req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("clone failed: %w", err))
		return
	}

	// Inspect the new container
	info, err := s.docker.InspectContainer(ctx, newDockerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	name := strings.TrimPrefix(info.Name, "/")
	newContainer, err := s.store.RegisterContainer(info.ID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Copy limits from source, reset usage
	if err := s.store.CopyLimits(containerID, newContainer.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Start enforcement for the clone
	s.enforcement.StartContainer(newContainer.ID, newContainer.DockerID)

	if s.bus != nil {
		s.bus.PublishData(events.ContainerRegister, newContainer.ID, events.ContainerRegisterData{
			DockerID: newContainer.DockerID,
			Name:     name,
		})
	}

	writeJSON(w, newContainer)
}

// --- In-container self-query endpoints ---

func (s *Server) handleSelfUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	containerID := s.resolveContainerFromRequest(r)
	if containerID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cannot identify container; use ?id=<container_id> or X-Container-ID header"))
		return
	}

	usage, err := s.store.GetAllUsage(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	limits, err := s.store.GetAllLimits(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, map[string]interface{}{
		"usage":  usage,
		"limits": limits,
	})
}

func (s *Server) handleSelfLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	containerID := s.resolveContainerFromRequest(r)
	if containerID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cannot identify container; use ?id=<container_id> or X-Container-ID header"))
		return
	}

	limits, err := s.store.GetAllLimits(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, limits)
}

func (s *Server) ipRefreshLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.refreshIPs()
		}
	}
}

func (s *Server) refreshIPs() {
	containers, err := s.store.ListContainers()
	if err != nil {
		log.Printf("[api] refreshIPs: list containers: %v", err)
		return
	}
	newMap := make(map[string]string, len(containers))
	ctx := context.Background()
	for _, c := range containers {
		ip, err := s.docker.ContainerIP(ctx, c.DockerID)
		if err != nil {
			continue
		}
		newMap[ip] = c.ID
	}
	s.ipMu.Lock()
	s.ipMap = newMap
	s.ipMu.Unlock()
}

func (s *Server) resolveContainerByIP(ip string) string {
	s.ipMu.RLock()
	defer s.ipMu.RUnlock()
	return s.ipMap[ip]
}

func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func (s *Server) handleSelfUsageByIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := extractIP(r.RemoteAddr)
	containerID := s.resolveContainerByIP(ip)
	if containerID == "" {
		writeError(w, http.StatusForbidden, fmt.Errorf("unknown container"))
		return
	}

	usage, err := s.store.GetAllUsage(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	limits, err := s.store.GetAllLimits(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, map[string]interface{}{
		"usage":  usage,
		"limits": limits,
	})
}

func (s *Server) handleSelfLimitsByIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := extractIP(r.RemoteAddr)
	containerID := s.resolveContainerByIP(ip)
	if containerID == "" {
		writeError(w, http.StatusForbidden, fmt.Errorf("unknown container"))
		return
	}

	limits, err := s.store.GetAllLimits(containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, limits)
}

func (s *Server) resolveContainerFromRequest(r *http.Request) string {
	// Try query parameter
	if id := r.URL.Query().Get("id"); id != "" {
		resolved, err := s.store.ResolveContainerID(id)
		if err == nil {
			return resolved
		}
	}
	// Try header
	if id := r.Header.Get("X-Container-ID"); id != "" {
		resolved, err := s.store.ResolveContainerID(id)
		if err == nil {
			return resolved
		}
	}
	return ""
}

func (s *Server) buildStatus(c model.Container) model.ContainerStatus {
	limits, _ := s.store.GetAllLimits(c.ID)
	usage, _ := s.store.GetAllUsage(c.ID)
	enforced := s.enforcement.GetEnforced(c.ID)

	state := "deleted"
	ctx := context.Background()
	info, err := s.docker.InspectContainer(ctx, c.DockerID)
	if err == nil {
		switch {
		case info.State.Paused:
			state = "paused"
		case info.State.Running:
			state = "running"
		default:
			state = "exited"
		}
	}

	var proxyAddr string
	if s.proxy != nil {
		proxyAddr = s.proxy.GetProxyAddr(c.ID)
	}

	return model.ContainerStatus{
		Container: c,
		Limits:    limits,
		Usage:     usage,
		Enforced:  enforced,
		State:     state,
		ProxyAddr: proxyAddr,
	}
}

// --- Ollama queue endpoints ---

func (s *Server) handleOllamaQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ollama == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("ollama not configured"))
		return
	}
	writeJSON(w, s.ollama.QueueStatus())
}

func (s *Server) handleOllamaModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ollama == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("ollama not configured"))
		return
	}
	writeJSON(w, map[string]interface{}{
		"models": s.ollama.AllowedModels(),
	})
}

func (s *Server) handleOllamaBidForContainer(w http.ResponseWriter, r *http.Request, containerID string) {
	if s.ollama == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("ollama not configured"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"bid": s.ollama.GetBid(containerID),
		})
	case http.MethodPut:
		var req struct {
			Bid int64 `json:"bid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.ollama.SetBid(containerID, req.Bid)
		writeJSON(w, map[string]interface{}{
			"bid": req.Bid,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleOllamaCancelForContainer(w http.ResponseWriter, r *http.Request, containerID string) {
	if s.ollama == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("ollama not configured"))
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ok := s.ollama.CancelEntry(containerID)
	writeJSON(w, map[string]interface{}{
		"cancelled": ok,
	})
}

func (s *Server) handleOllamaBidByIP(w http.ResponseWriter, r *http.Request) {
	if s.ollama == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("ollama not configured"))
		return
	}

	ip := extractIP(r.RemoteAddr)
	containerID := s.resolveContainerByIP(ip)
	if containerID == "" {
		writeError(w, http.StatusForbidden, fmt.Errorf("unknown container"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"bid": s.ollama.GetBid(containerID),
		})
	case http.MethodPut:
		var req struct {
			Bid int64 `json:"bid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.ollama.SetBid(containerID, req.Bid)
		writeJSON(w, map[string]interface{}{
			"bid": req.Bid,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Provider management ---

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Return provider enabled states
		result := map[string]interface{}{
			"ollama_available": s.ollama != nil,
		}
		if st, ok := s.proxy.(*proxy.SpendingTracker); ok {
			result["enabled"] = st.GetEnabledHosts()
		}
		writeJSON(w, result)
	case http.MethodPut:
		st, ok := s.proxy.(*proxy.SpendingTracker)
		if !ok {
			writeError(w, http.StatusNotImplemented, fmt.Errorf("provider management not available"))
			return
		}
		var req map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		for host, enabled := range req {
			st.EnableHost(host, enabled)
		}
		writeJSON(w, map[string]interface{}{
			"enabled": st.GetEnabledHosts(),
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Runtime config management ---

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.buildConfigResponse())
	case http.MethodPut:
		st, ok := s.proxy.(*proxy.SpendingTracker)
		if !ok {
			writeError(w, http.StatusNotImplemented, fmt.Errorf("config management not available"))
			return
		}
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.applyConfig(st, req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, s.buildConfigResponse())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) buildConfigResponse() map[string]interface{} {
	result := map[string]interface{}{}

	if st, ok := s.proxy.(*proxy.SpendingTracker); ok {
		enabled := st.GetEnabledHosts()
		result["anthropic_enabled"] = enabled["api.anthropic.com"]
		result["openai_enabled"] = enabled["api.openai.com"]
		result["ollama_enabled"] = enabled["ollama"]
		result["anthropic_key_set"] = st.HasAPIKey("api.anthropic.com")
		result["openai_key_set"] = st.HasAPIKey("api.openai.com")
	}

	if s.ollama != nil {
		cfg := s.ollama.GetConfig()
		result["ollama_url"] = cfg.OllamaURL
		result["ollama_models"] = cfg.AllowedModels
		result["ollama_queue_size"] = cfg.MaxQueueSize
		result["ollama_timeout"] = cfg.RequestTimeout.String()
		result["ollama_default_bid"] = cfg.DefaultBid
	}

	return result
}

func (s *Server) applyConfig(st *proxy.SpendingTracker, req map[string]interface{}) error {
	for key, val := range req {
		switch key {
		case "anthropic_enabled":
			b, ok := val.(bool)
			if !ok {
				return fmt.Errorf("anthropic_enabled: expected bool")
			}
			st.EnableHost("api.anthropic.com", b)
		case "openai_enabled":
			b, ok := val.(bool)
			if !ok {
				return fmt.Errorf("openai_enabled: expected bool")
			}
			st.EnableHost("api.openai.com", b)
		case "ollama_enabled":
			b, ok := val.(bool)
			if !ok {
				return fmt.Errorf("ollama_enabled: expected bool")
			}
			st.EnableHost("ollama", b)
		case "anthropic_key":
			keyVal, ok := val.(string)
			if !ok {
				return fmt.Errorf("anthropic_key: expected string")
			}
			st.SetAPIKey("api.anthropic.com", keyVal)
		case "openai_key":
			keyVal, ok := val.(string)
			if !ok {
				return fmt.Errorf("openai_key: expected string")
			}
			st.SetAPIKey("api.openai.com", keyVal)
		case "ollama_models":
			if s.ollama == nil {
				return fmt.Errorf("ollama not configured")
			}
			arr, ok := val.([]interface{})
			if !ok {
				return fmt.Errorf("ollama_models: expected array of strings")
			}
			models := make([]string, 0, len(arr))
			for _, v := range arr {
				m, ok := v.(string)
				if !ok {
					return fmt.Errorf("ollama_models: expected array of strings")
				}
				models = append(models, m)
			}
			s.ollama.SetAllowedModels(models)
		case "ollama_queue_size":
			if s.ollama == nil {
				return fmt.Errorf("ollama not configured")
			}
			n, ok := val.(float64)
			if !ok {
				return fmt.Errorf("ollama_queue_size: expected number")
			}
			s.ollama.SetMaxQueueSize(int(n))
		case "ollama_timeout":
			if s.ollama == nil {
				return fmt.Errorf("ollama not configured")
			}
			str, ok := val.(string)
			if !ok {
				return fmt.Errorf("ollama_timeout: expected duration string")
			}
			d, err := time.ParseDuration(str)
			if err != nil {
				return fmt.Errorf("ollama_timeout: %w", err)
			}
			s.ollama.SetRequestTimeout(d)
		case "ollama_default_bid":
			if s.ollama == nil {
				return fmt.Errorf("ollama not configured")
			}
			n, ok := val.(float64)
			if !ok {
				return fmt.Errorf("ollama_default_bid: expected number")
			}
			s.ollama.SetDefaultBid(int64(n))
		default:
			return fmt.Errorf("unknown config key: %s", key)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

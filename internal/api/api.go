package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/keneo/docker-dynamic-limits/internal/docker"
	"github.com/keneo/docker-dynamic-limits/internal/enforcement"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/proxy"
	"github.com/keneo/docker-dynamic-limits/internal/store"
)

// Server is the REST API server for ddld.
type Server struct {
	store       store.DataStore
	docker      docker.DockerClient
	enforcement enforcement.EnforcementController
	proxy       proxy.SpendingProxy
	mux         *http.ServeMux
}

// NewServer creates a new API server.
func NewServer(st store.DataStore, dc docker.DockerClient, em enforcement.EnforcementController, px proxy.SpendingProxy) *Server {
	s := &Server{
		store:       st,
		docker:      dc,
		enforcement: em,
		proxy:       px,
		mux:         http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/containers", s.handleContainers)
	s.mux.HandleFunc("/containers/", s.handleContainer)
	s.mux.HandleFunc("/register", s.handleRegister)
	// In-container query endpoints (container identifies itself by source IP or token)
	s.mux.HandleFunc("/usage", s.handleSelfUsage)
	s.mux.HandleFunc("/limits", s.handleSelfLimits)
}

// Handler returns the HTTP handler (full API).
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ReadOnlyHandler returns an HTTP handler with only read-only, guest-facing routes.
// This is intended for the TCP listener that containers can reach.
func (s *Server) ReadOnlyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers", s.handleContainersReadOnly)
	mux.HandleFunc("/usage", s.handleSelfUsage)
	mux.HandleFunc("/limits", s.handleSelfLimits)
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

	// Set up spending proxy if needed
	if s.proxy != nil {
		proxyAddr, err := s.proxy.RegisterContainer(c.ID, 0, 0)
		if err != nil {
			log.Printf("[api] warning: failed to start proxy for %s: %v", c.ID, err)
		} else {
			log.Printf("[api] proxy for %s available at %s", c.ID, proxyAddr)
		}
	}

	writeJSON(w, c)
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

	switch parts[1] {
	case "limits":
		s.handleLimits(w, r, containerID)
	case "usage":
		s.handleUsage(w, r, containerID)
	case "clone":
		s.handleClone(w, r, containerID)
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
		if err := s.store.RemoveContainer(containerID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
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
		var newValue int64

		switch req.Operation {
		case "set", "":
			newValue = req.Value
		case "increase":
			current, _ := s.store.GetLimit(containerID, lt)
			newValue = current + req.Value
		case "decrease":
			current, _ := s.store.GetLimit(containerID, lt)
			newValue = current - req.Value
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

	return model.ContainerStatus{
		Container: c,
		Limits:    limits,
		Usage:     usage,
		Enforced:  enforced,
	}
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

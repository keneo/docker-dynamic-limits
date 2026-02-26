package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

// SpendingProxy defines the interface for spending tracking operations.
type SpendingProxy interface {
	RegisterContainer(containerID string, budget int64, existingSpending int64) (string, error)
	UpdateBudget(containerID string, budget int64)
	GetSpending(containerID string) int64
	SetSpending(containerID string, cents int64)
	GetProxyAddr(containerID string) string
}

// SpendingTracker tracks API spending per container via an HTTP forward proxy.
type SpendingTracker struct {
	mu       sync.RWMutex
	// containerByAddr maps proxy listener address (ip:port) to container ID
	containerByAddr map[string]string
	// proxyAddrs maps container ID to its proxy listener address
	proxyAddrs map[string]string
	// spending maps container ID to cumulative spending in cents
	spending map[string]int64
	// budgets maps container ID to budget limit in cents
	budgets map[string]int64
	// prices maps model name to price-per-token in micro-cents (1/1_000_000 of a cent)
	prices map[string]ModelPricing
	// apiKeys maps hostname (e.g. "api.anthropic.com") to API key
	apiKeys map[string]string
	// onSpendingUpdate is called when spending changes (to persist to store)
	onSpendingUpdate func(containerID string, totalCents int64)
	// transport is the HTTP transport used for outgoing requests (nil = http.DefaultTransport)
	transport http.RoundTripper
}

// ModelPricing holds per-token costs in micro-cents.
type ModelPricing struct {
	InputPerToken  int64 `json:"input_per_token"`
	OutputPerToken int64 `json:"output_per_token"`
}

// NewSpendingTracker creates a new spending tracker.
func NewSpendingTracker(onUpdate func(containerID string, totalCents int64)) *SpendingTracker {
	return &SpendingTracker{
		containerByAddr:  make(map[string]string),
		proxyAddrs:       make(map[string]string),
		spending:         make(map[string]int64),
		budgets:          make(map[string]int64),
		prices:           defaultPrices(),
		apiKeys:          make(map[string]string),
		onSpendingUpdate: onUpdate,
	}
}

func defaultPrices() map[string]ModelPricing {
	// Prices in micro-cents per token (1 cent = 1_000_000 micro-cents)
	// These are approximate — users can override via config
	return map[string]ModelPricing{
		// OpenAI
		"gpt-4":         {InputPerToken: 3000, OutputPerToken: 6000},   // $0.03/$0.06 per 1K
		"gpt-4-turbo":   {InputPerToken: 1000, OutputPerToken: 3000},   // $0.01/$0.03 per 1K
		"gpt-4o":        {InputPerToken: 250, OutputPerToken: 1000},    // $0.0025/$0.01 per 1K
		"gpt-4o-mini":   {InputPerToken: 15, OutputPerToken: 60},       // $0.00015/$0.0006 per 1K
		"gpt-3.5-turbo": {InputPerToken: 50, OutputPerToken: 150},      // $0.0005/$0.0015 per 1K
		// Anthropic
		"claude-3-opus":   {InputPerToken: 1500, OutputPerToken: 7500},  // $0.015/$0.075 per 1K
		"claude-3-sonnet": {InputPerToken: 300, OutputPerToken: 1500},   // $0.003/$0.015 per 1K
		"claude-3-haiku":  {InputPerToken: 25, OutputPerToken: 125},     // $0.00025/$0.00125 per 1K
		"claude-haiku-4-5": {InputPerToken: 80, OutputPerToken: 400},   // $0.0008/$0.004 per 1K
	}
}

// RegisterContainer sets up proxy tracking for a container.
// Returns the proxy address to use as HTTP_PROXY.
func (st *SpendingTracker) RegisterContainer(containerID string, budget int64, existingSpending int64) (string, error) {
	// Start a per-container proxy listener
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	addr := listener.Addr().String()

	st.mu.Lock()
	st.containerByAddr[addr] = containerID
	st.proxyAddrs[containerID] = addr
	st.spending[containerID] = existingSpending
	st.budgets[containerID] = budget
	st.mu.Unlock()

	proxy := &http.Server{
		Handler: st.proxyHandler(containerID),
	}
	go proxy.Serve(listener)

	log.Printf("[proxy] started proxy for container %s on %s (budget: %d cents)", containerID, addr, budget)
	return addr, nil
}

// UpdateBudget changes the spending budget for a container.
func (st *SpendingTracker) UpdateBudget(containerID string, budget int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.budgets[containerID] = budget
}

// GetSpending returns current spending in cents for a container.
func (st *SpendingTracker) GetSpending(containerID string) int64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.spending[containerID]
}

// SetSpending sets the current spending for a container (e.g., after loading from store).
func (st *SpendingTracker) SetSpending(containerID string, cents int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.spending[containerID] = cents
}

// GetProxyAddr returns the proxy listener address for a container.
func (st *SpendingTracker) GetProxyAddr(containerID string) string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.proxyAddrs[containerID]
}

// SetAPIKeys configures API keys for tracked hosts.
// Keys map hostname (e.g. "api.anthropic.com") to the API key string.
func (st *SpendingTracker) SetAPIKeys(keys map[string]string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for k, v := range keys {
		st.apiKeys[k] = v
	}
}

func (st *SpendingTracker) proxyHandler(containerID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if budget is exceeded before proxying
		st.mu.RLock()
		budget := st.budgets[containerID]
		spent := st.spending[containerID]
		st.mu.RUnlock()

		isAPICall := st.isTrackedAPI(r.Host)

		if isAPICall && budget > 0 && spent >= budget {
			http.Error(w, `{"error":"spending budget exceeded"}`, http.StatusTooManyRequests)
			return
		}

		// Forward the request
		// For tracked API hosts with configured keys, upgrade to HTTPS and inject auth
		st.mu.RLock()
		apiKey := ""
		if isAPICall {
			host := stripPort(r.Host)
			apiKey = st.apiKeys[host]
		}
		st.mu.RUnlock()

		var targetURL string
		if isAPICall && apiKey != "" {
			// Build HTTPS URL explicitly — relay HTTP→HTTPS
			host := stripPort(r.Host)
			targetURL = "https://" + host + r.URL.Path
			if r.URL.RawQuery != "" {
				targetURL += "?" + r.URL.RawQuery
			}
		} else {
			targetURL = r.URL.String()
		}

		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		outReq.Header = r.Header.Clone()
		outReq.ContentLength = r.ContentLength

		// When relaying with API keys, strip container-provided auth and inject ours
		if isAPICall && apiKey != "" {
			outReq.Header.Del("Authorization")
			outReq.Header.Del("x-api-key")
			host := stripPort(r.Host)
			switch host {
			case "api.anthropic.com":
				outReq.Header.Set("x-api-key", apiKey)
				if outReq.Header.Get("anthropic-version") == "" {
					outReq.Header.Set("anthropic-version", "2023-06-01")
				}
			case "api.openai.com":
				outReq.Header.Set("Authorization", "Bearer "+apiKey)
			}
		}

		transport := st.transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// If it's a tracked API, read the body to extract usage
		var body []byte
		if isAPICall {
			body, err = io.ReadAll(resp.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			st.trackSpending(containerID, r.Host, body)
		}

		// Copy response headers
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		if isAPICall {
			w.Write(body)
		} else {
			io.Copy(w, resp.Body)
		}
	})
}

// IsTrackedAPIHost returns true if the host is a tracked API endpoint.
func IsTrackedAPIHost(host string) bool {
	tracked := []string{
		"api.openai.com",
		"api.anthropic.com",
	}
	for _, h := range tracked {
		if strings.Contains(host, h) {
			return true
		}
	}
	return false
}

func (st *SpendingTracker) isTrackedAPI(host string) bool {
	return IsTrackedAPIHost(host)
}

// apiUsage is the common usage structure in API responses.
type apiUsage struct {
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

func (st *SpendingTracker) trackSpending(containerID string, host string, body []byte) {
	var resp apiUsage
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}

	inputTokens := resp.Usage.PromptTokens + resp.Usage.InputTokens
	outputTokens := resp.Usage.CompletionTokens + resp.Usage.OutputTokens

	if inputTokens == 0 && outputTokens == 0 {
		return
	}

	// Find pricing for this model
	modelName := normalizeModelName(resp.Model)
	st.mu.RLock()
	pricing, ok := st.prices[modelName]
	st.mu.RUnlock()
	if !ok {
		// Use a conservative default
		pricing = ModelPricing{InputPerToken: 1000, OutputPerToken: 3000}
	}

	costCents := CalculateSpendingCents(inputTokens, outputTokens, pricing)

	st.mu.Lock()
	st.spending[containerID] += costCents
	newTotal := st.spending[containerID]
	st.mu.Unlock()

	if st.onSpendingUpdate != nil {
		st.onSpendingUpdate(containerID, newTotal)
	}

	log.Printf("[proxy] container %s: %d input + %d output tokens (%s) = %d cents (total: %d)",
		containerID, inputTokens, outputTokens, resp.Model, costCents, newTotal)
}

// CalculateSpendingCents calculates the cost in cents from token counts and pricing.
func CalculateSpendingCents(inputTokens, outputTokens int64, pricing ModelPricing) int64 {
	costMicroCents := inputTokens*pricing.InputPerToken + outputTokens*pricing.OutputPerToken
	costCents := costMicroCents / 1_000_000
	if costCents == 0 && costMicroCents > 0 {
		costCents = 1 // minimum 1 cent charge to avoid free-riding
	}
	return costCents
}

// stripPort removes the port suffix from a host string (e.g. "api.openai.com:8080" -> "api.openai.com").
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func normalizeModelName(model string) string {
	// Strip date suffixes and version details
	model = strings.ToLower(model)
	// Map known prefixes
	prefixes := []string{
		"gpt-4o-mini", "gpt-4o", "gpt-4-turbo", "gpt-4", "gpt-3.5-turbo",
		"claude-haiku-4-5", "claude-3-opus", "claude-3-sonnet", "claude-3-haiku",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(model, p) {
			return p
		}
	}
	return model
}

// ConnectHandler returns an HTTP handler that handles CONNECT method for HTTPS proxying.
func (st *SpendingTracker) ConnectHandler(containerID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			st.handleConnect(containerID, w, r)
			return
		}
		st.proxyHandler(containerID).ServeHTTP(w, r)
	})
}

func (st *SpendingTracker) handleConnect(containerID string, w http.ResponseWriter, r *http.Request) {
	// For CONNECT (HTTPS), we just tunnel the connection
	// We can't inspect the content, but we can block by host
	st.mu.RLock()
	budget := st.budgets[containerID]
	spent := st.spending[containerID]
	st.mu.RUnlock()

	if st.isTrackedAPI(r.Host) && budget > 0 && spent >= budget {
		http.Error(w, "spending budget exceeded", http.StatusTooManyRequests)
		return
	}

	destConn, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer destConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() { io.Copy(destConn, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, destConn); done <- struct{}{} }()
	<-done
}

// LoadPrices loads custom pricing from a JSON reader.
func (st *SpendingTracker) LoadPrices(r io.Reader) error {
	var prices map[string]ModelPricing
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &prices); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for k, v := range prices {
		st.prices[k] = v
	}
	return nil
}

// SetResolveOverrides configures DNS resolution overrides so that requests
// to the given hostnames are routed to the specified IP addresses instead.
// This enables testing with mock API servers on localhost.
func (st *SpendingTracker) SetResolveOverrides(overrides map[string]string) {
	st.transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil {
				if override, ok := overrides[host]; ok {
					addr = net.JoinHostPort(override, port)
				}
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}


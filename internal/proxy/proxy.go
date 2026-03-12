package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SpendingProxy defines the interface for spending tracking operations.
type SpendingProxy interface {
	RegisterContainer(containerID string, budget int64, existingSpending int64) (string, error)
	UpdateBudget(containerID string, budget int64)
	GetSpending(containerID string) int64
	SetSpending(containerID string, cents int64)
	GetProxyAddr(containerID string) string
	AddSpending(containerID string, milliCents int64)
	GetActivity(containerID string) []ProxyActivity
	RecordActivity(containerID string, a ProxyActivity)
}

// OllamaHandler is the interface the proxy uses to dispatch Ollama requests.
type OllamaHandler interface {
	HandleRequest(containerID string, w http.ResponseWriter, r *http.Request)
	OllamaHost() string
}

// SpendingTracker tracks API spending per container via an HTTP forward proxy.
type SpendingTracker struct {
	mu       sync.RWMutex
	// containerByAddr maps proxy listener address (ip:port) to container ID
	containerByAddr map[string]string
	// proxyAddrs maps container ID to its proxy listener address
	proxyAddrs map[string]string
	// spending maps container ID to cumulative spending in micro-cents (1 cent = 1_000_000 micro-cents)
	spending map[string]int64
	// budgets maps container ID to budget limit in micro-cents (1 cent = 1_000_000 micro-cents)
	budgets map[string]int64
	// prices maps model name to price-per-token in micro-cents (1/1_000_000 of a cent)
	prices map[string]ModelPricing
	// apiKeys maps hostname (e.g. "api.anthropic.com") to API key
	apiKeys map[string]string
	// onSpendingUpdate is called when spending changes (to persist to store)
	onSpendingUpdate func(containerID string, totalCents int64)
	// transport is the HTTP transport used for outgoing requests (nil = http.DefaultTransport)
	transport http.RoundTripper
	// enabledHosts tracks which provider hosts are enabled for proxying
	enabledHosts map[string]bool
	// ollamaQueue handles Ollama inference requests (nil when Ollama not configured)
	ollamaQueue OllamaHandler
	// activity stores recent proxy requests per container
	activity map[string][]ProxyActivity
}

// ModelPricing holds per-token costs in micro-cents.
type ModelPricing struct {
	InputPerToken  int64 `json:"input_per_token"`
	OutputPerToken int64 `json:"output_per_token"`
}

// ProxyActivity records a single proxied request.
type ProxyActivity struct {
	Timestamp    time.Time `json:"timestamp"`
	Host         string    `json:"host"`
	Path         string    `json:"path"`
	Method       string    `json:"method"`
	RequestBody  string    `json:"request_body"`
	StatusCode   int       `json:"status_code"`
	ResponseBody string    `json:"response_body"`
	Model        string    `json:"model"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CostMicro    int64     `json:"cost_micro"`
	DurationMs   int64     `json:"duration_ms"`
	Error        string    `json:"error,omitempty"`
}

const (
	maxActivityPerContainer = 20
	maxBodySize             = 4096
)

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
		enabledHosts:     make(map[string]bool),
		activity:         make(map[string][]ProxyActivity),
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

// SeedSpending pre-loads spending totals (in milli-cents) so that
// incremental updates don't start from zero after a daemon restart.
func (st *SpendingTracker) SeedSpending(totals map[string]int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, milliCents := range totals {
		st.spending[id] = milliCents * 1_000 // milli-cents → micro-cents
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
	st.spending[containerID] = existingSpending * 1_000 // milli-cents → micro-cents
	st.budgets[containerID] = budget * 1_000            // milli-cents → micro-cents
	st.mu.Unlock()

	proxy := &http.Server{
		Handler: st.proxyHandler(containerID),
	}
	go proxy.Serve(listener)

	log.Printf("[proxy] started proxy for container %s on %s (budget: %d milli-cents)", containerID, addr, budget)
	return addr, nil
}

// UpdateBudget changes the spending budget for a container (budget in milli-cents).
func (st *SpendingTracker) UpdateBudget(containerID string, budget int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.budgets[containerID] = budget * 1_000 // milli-cents → micro-cents
}

// GetSpending returns current spending in milli-cents for a container.
func (st *SpendingTracker) GetSpending(containerID string) int64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.spending[containerID] / 1_000 // micro-cents → milli-cents
}

// SetSpending sets the current spending for a container (milli-cents, e.g., after loading from store).
func (st *SpendingTracker) SetSpending(containerID string, milliCents int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.spending[containerID] = milliCents * 1_000 // milli-cents → micro-cents
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

// SetAPIKey sets or updates a single API key for a host.
func (st *SpendingTracker) SetAPIKey(host string, key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.apiKeys[host] = key
}

// HasAPIKey returns true if an API key is configured for the given host.
func (st *SpendingTracker) HasAPIKey(host string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.apiKeys[host] != ""
}

// AddSpending adds milliCents to the spending for a container.
func (st *SpendingTracker) AddSpending(containerID string, milliCents int64) {
	st.mu.Lock()
	st.spending[containerID] += milliCents * 1_000 // milli-cents → micro-cents
	newTotal := st.spending[containerID]
	st.mu.Unlock()
	if st.onSpendingUpdate != nil {
		st.onSpendingUpdate(containerID, newTotal/1_000)
	}
}

// SetOllamaHandler sets the handler for Ollama inference requests.
func (st *SpendingTracker) SetOllamaHandler(h OllamaHandler) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ollamaQueue = h
}

// SetEnabledHosts sets which provider hosts are enabled.
func (st *SpendingTracker) SetEnabledHosts(hosts map[string]bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.enabledHosts = make(map[string]bool)
	for k, v := range hosts {
		st.enabledHosts[k] = v
	}
}

// EnableHost enables or disables a provider host at runtime.
func (st *SpendingTracker) EnableHost(host string, enabled bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.enabledHosts[host] = enabled
}

// GetEnabledHosts returns a copy of the enabled hosts map.
func (st *SpendingTracker) GetEnabledHosts() map[string]bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	result := make(map[string]bool)
	for k, v := range st.enabledHosts {
		result[k] = v
	}
	return result
}

// RecordActivity appends a proxy activity entry for a container.
func (st *SpendingTracker) RecordActivity(containerID string, a ProxyActivity) {
	st.mu.Lock()
	defer st.mu.Unlock()
	entries := st.activity[containerID]
	entries = append(entries, a)
	if len(entries) > maxActivityPerContainer {
		entries = entries[len(entries)-maxActivityPerContainer:]
	}
	st.activity[containerID] = entries
}

// GetActivity returns recent proxy activity for a container (most recent last).
func (st *SpendingTracker) GetActivity(containerID string) []ProxyActivity {
	st.mu.RLock()
	defer st.mu.RUnlock()
	entries := st.activity[containerID]
	result := make([]ProxyActivity, len(entries))
	copy(result, entries)
	return result
}

// truncateBody truncates a byte slice to maxBodySize and returns it as a string.
func truncateBody(b []byte) string {
	if len(b) <= maxBodySize {
		return string(b)
	}
	return string(b[:maxBodySize]) + "..."
}

func (st *SpendingTracker) isOllamaHost(host string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.ollamaQueue == nil || !st.enabledHosts["ollama"] {
		return false
	}
	return stripPort(host) == st.ollamaQueue.OllamaHost()
}

// isDisabledProvider returns true if the host is a known provider that is explicitly disabled.
func (st *SpendingTracker) isDisabledProvider(host string) bool {
	h := stripPort(host)
	st.mu.RLock()
	defer st.mu.RUnlock()
	// Check cloud API hosts
	if IsTrackedAPIHost(h) {
		if len(st.enabledHosts) > 0 && !st.enabledHosts[h] {
			return true
		}
	}
	// Check Ollama host
	if st.ollamaQueue != nil && h == st.ollamaQueue.OllamaHost() && !st.enabledHosts["ollama"] {
		return true
	}
	return false
}

func (st *SpendingTracker) proxyHandler(containerID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block requests to disabled providers
		if st.isDisabledProvider(r.Host) {
			st.RecordActivity(containerID, ProxyActivity{
				Timestamp:  time.Now(),
				Host:       stripPort(r.Host),
				Path:       r.URL.Path,
				Method:     r.Method,
				StatusCode: http.StatusForbidden,
				Error:      "provider disabled in ddl proxy",
			})
			http.Error(w, `{"error":"provider disabled in ddl proxy"}`, http.StatusForbidden)
			return
		}

		// Dispatch Ollama requests to the queue handler
		if st.isOllamaHost(r.Host) {
			st.mu.RLock()
			oq := st.ollamaQueue
			st.mu.RUnlock()
			if oq != nil {
				oq.HandleRequest(containerID, w, r)
				return
			}
		}

		// Check if budget is exceeded before proxying
		st.mu.RLock()
		budget := st.budgets[containerID]
		spent := st.spending[containerID]
		st.mu.RUnlock()

		isAPICall := st.isTrackedAPI(r.Host)

		if isAPICall && budget > 0 && spent >= budget {
			st.RecordActivity(containerID, ProxyActivity{
				Timestamp:  time.Now(),
				Host:       stripPort(r.Host),
				Path:       r.URL.Path,
				Method:     r.Method,
				StatusCode: http.StatusTooManyRequests,
				Error:      "spending budget exceeded",
			})
			http.Error(w, `{"error":"spending budget exceeded"}`, http.StatusTooManyRequests)
			return
		}

		// Buffer request body for tracked API calls (needed for activity recording)
		var reqBody []byte
		if isAPICall && r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
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
		start := time.Now()
		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		durationMs := time.Since(start).Milliseconds()

		// If it's a tracked API, read the body to extract usage
		var body []byte
		if isAPICall {
			body, err = io.ReadAll(resp.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			act := st.trackSpendingWithActivity(containerID, r.Host, r.URL.Path, r.Method, reqBody, body, resp.StatusCode, durationMs)
			st.RecordActivity(containerID, act)
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
	if !IsTrackedAPIHost(host) {
		return false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	// If enabledHosts is empty (no explicit config), fall back to checking IsTrackedAPIHost only
	if len(st.enabledHosts) == 0 {
		return true
	}
	h := stripPort(host)
	return st.enabledHosts[h]
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

// trackSpending is a convenience wrapper for tests that don't need activity data.
func (st *SpendingTracker) trackSpending(containerID string, host string, respBody []byte) {
	st.trackSpendingWithActivity(containerID, host, "", "", nil, respBody, 0, 0)
}

func (st *SpendingTracker) trackSpendingWithActivity(containerID string, host string, path string, method string, reqBody []byte, respBody []byte, statusCode int, durationMs int64) ProxyActivity {
	act := ProxyActivity{
		Timestamp:    time.Now(),
		Host:         stripPort(host),
		Path:         path,
		Method:       method,
		RequestBody:  truncateBody(reqBody),
		StatusCode:   statusCode,
		ResponseBody: truncateBody(respBody),
		DurationMs:   durationMs,
	}

	var resp apiUsage
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return act
	}

	inputTokens := resp.Usage.PromptTokens + resp.Usage.InputTokens
	outputTokens := resp.Usage.CompletionTokens + resp.Usage.OutputTokens
	act.Model = resp.Model
	act.InputTokens = inputTokens
	act.OutputTokens = outputTokens

	if inputTokens == 0 && outputTokens == 0 {
		return act
	}

	// Find pricing for this model
	modelName := normalizeModelName(resp.Model)
	st.mu.RLock()
	pricing, ok := st.prices[modelName]
	st.mu.RUnlock()
	if !ok {
		pricing = ModelPricing{InputPerToken: 1000, OutputPerToken: 3000}
	}

	costMicroCents := CalculateSpendingMicroCents(inputTokens, outputTokens, pricing)
	act.CostMicro = costMicroCents

	st.mu.Lock()
	st.spending[containerID] += costMicroCents
	newTotal := st.spending[containerID]
	st.mu.Unlock()

	if st.onSpendingUpdate != nil {
		st.onSpendingUpdate(containerID, newTotal/1_000)
	}

	log.Printf("[proxy] container %s: %d input + %d output tokens (%s) = %d micro-cents (total: %d milli-cents)",
		containerID, inputTokens, outputTokens, resp.Model, costMicroCents, newTotal/1_000)

	return act
}

// CalculateSpendingMicroCents calculates the cost in micro-cents from token counts and pricing.
func CalculateSpendingMicroCents(inputTokens, outputTokens int64, pricing ModelPricing) int64 {
	return inputTokens*pricing.InputPerToken + outputTokens*pricing.OutputPerToken
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


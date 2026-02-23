package proxy

import (
	"context"
	"encoding/json"
	"io"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// localRedirectTransport returns an http.Transport that resolves the given
// hostname to 127.0.0.1, so requests targeting e.g. api.openai.com:PORT
// actually reach a local mock server on that port.
func localRedirectTransport(hosts ...string) *http.Transport {
	hostSet := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		hostSet[h] = true
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil && hostSet[host] {
				addr = "127.0.0.1:" + port
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		},
	}
}

// proxyClient creates an http.Client configured to use the given proxy address.
func proxyClient(proxyAddr string) *http.Client {
	proxyURL, _ := url.Parse(fmt.Sprintf("http://%s", proxyAddr))
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 5 * time.Second,
	}
}

// mockOpenAIServer starts an httptest server that returns OpenAI-style responses.
func mockOpenAIServer(model string, promptTokens, completionTokens int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    "chatcmpl-test",
			"model": model,
			"usage": map[string]int64{
				"prompt_tokens":     promptTokens,
				"completion_tokens": completionTokens,
			},
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": "hello"}},
			},
		})
	}))
}

// mockAnthropicServer starts an httptest server that returns Anthropic-style responses.
func mockAnthropicServer(model string, inputTokens, outputTokens int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    "msg_test",
			"type":  "message",
			"model": model,
			"usage": map[string]int64{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			},
			"content": []map[string]interface{}{
				{"type": "text", "text": "hello"},
			},
		})
	}))
}

func mockPort(s *httptest.Server) int {
	return s.Listener.Addr().(*net.TCPAddr).Port
}

func TestProxyE2E_BasicSpendingTracking(t *testing.T) {
	mock := mockOpenAIServer("gpt-4o", 1000, 500)
	defer mock.Close()
	port := mockPort(mock)

	var callbackContainer string
	var callbackTotal int64
	var updateCount int32

	st := NewSpendingTracker(func(containerID string, totalCents int64) {
		callbackContainer = containerID
		callbackTotal = totalCents
		atomic.AddInt32(&updateCount, 1)
	})
	st.transport = localRedirectTransport("api.openai.com")

	proxyAddr, err := st.RegisterContainer("test-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	// Send a request through the proxy
	resp, err := client.Post(apiURL, "application/json",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	spending := st.GetSpending("test-c1")
	if spending <= 0 {
		t.Errorf("spending = %d, want > 0 after API call", spending)
	}
	if callbackContainer != "test-c1" {
		t.Errorf("callback containerID = %q, want %q", callbackContainer, "test-c1")
	}
	if callbackTotal != spending {
		t.Errorf("callback total = %d, spending = %d, should match", callbackTotal, spending)
	}
	if atomic.LoadInt32(&updateCount) != 1 {
		t.Errorf("updateCount = %d, want 1", updateCount)
	}

	t.Logf("spending after 1 request: %d cents", spending)
}

func TestProxyE2E_CumulativeSpending(t *testing.T) {
	mock := mockOpenAIServer("gpt-4o", 1000, 500)
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	proxyAddr, err := st.RegisterContainer("cumul-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	// First request
	resp, err := client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	resp.Body.Close()
	first := st.GetSpending("cumul-c1")

	// Second request
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	resp.Body.Close()
	second := st.GetSpending("cumul-c1")

	// Third request
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("third request: %v", err)
	}
	resp.Body.Close()
	third := st.GetSpending("cumul-c1")

	if first <= 0 {
		t.Errorf("first = %d, want > 0", first)
	}
	if second <= first {
		t.Errorf("second (%d) should be > first (%d)", second, first)
	}
	if third <= second {
		t.Errorf("third (%d) should be > second (%d)", third, second)
	}

	t.Logf("spending progression: %d -> %d -> %d cents", first, second, third)
}

func TestProxyE2E_BudgetEnforcementBlocks(t *testing.T) {
	// gpt-4: input 3000, output 6000 micro-cents/token
	// 10000 input * 3000 + 5000 output * 6000 = 30M + 30M = 60M micro-cents = 60 cents per call
	mock := mockOpenAIServer("gpt-4", 10000, 5000)
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	// Budget of 100 cents ($1.00)
	proxyAddr, err := st.RegisterContainer("budget-c1", 100, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	// 1st request: spending 0 < 100 budget → allowed. After: ~60 cents
	resp, err := client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("1st request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("1st request status = %d, want 200", resp.StatusCode)
	}

	after1 := st.GetSpending("budget-c1")
	t.Logf("after 1st: %d cents (budget: 100)", after1)

	// 2nd request: spending ~60 < 100 → allowed. After: ~120 cents
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("2nd request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("2nd request status = %d, want 200", resp.StatusCode)
	}

	after2 := st.GetSpending("budget-c1")
	t.Logf("after 2nd: %d cents (budget: 100)", after2)
	if after2 < 100 {
		t.Fatalf("expected spending >= 100 to trigger block, got %d", after2)
	}

	// 3rd request: spending >= 100 → BLOCKED
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("3rd request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("3rd request status = %d, want 429; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "spending budget exceeded") {
		t.Errorf("expected budget exceeded message, got: %s", body)
	}

	// Spending should NOT increase (request was blocked before proxying)
	after3 := st.GetSpending("budget-c1")
	if after3 != after2 {
		t.Errorf("spending changed from %d to %d despite block", after2, after3)
	}
}

func TestProxyE2E_BudgetIncreaseUnblocks(t *testing.T) {
	mock := mockOpenAIServer("gpt-4", 10000, 5000) // ~60 cents per call
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	proxyAddr, err := st.RegisterContainer("unblock-c1", 50, 0) // very tight budget
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	// 1st request: spending 0 < 50 → allowed. After: ~60 cents
	resp, err := client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("1st request: %v", err)
	}
	resp.Body.Close()

	// 2nd request: spending ~60 >= 50 → blocked
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("2nd request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	// Increase budget to 500
	st.UpdateBudget("unblock-c1", 500)

	// Now request should succeed
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request after budget increase: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after budget increase: status = %d, want 200", resp.StatusCode)
	}
}

func TestProxyE2E_NonTrackedHostNotTracked(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello from example.com"))
	}))
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("example.com")

	proxyAddr, err := st.RegisterContainer("notrack-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)

	resp, err := client.Get(fmt.Sprintf("http://example.com:%d/hello", port))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "hello from example.com" {
		t.Errorf("body = %q, want %q", body, "hello from example.com")
	}

	spending := st.GetSpending("notrack-c1")
	if spending != 0 {
		t.Errorf("spending = %d, want 0 for non-tracked host", spending)
	}
}

func TestProxyE2E_NonTrackedHostIgnoresBudget(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("example.com")

	// Even with budget exceeded, non-tracked hosts should pass through
	proxyAddr, err := st.RegisterContainer("nobudget-c1", 10, 100) // spending(100) > budget(10)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)

	resp, err := client.Get(fmt.Sprintf("http://example.com:%d/", port))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("non-tracked host should not be budget-blocked, got status %d", resp.StatusCode)
	}
}

func TestProxyE2E_AnthropicAPI(t *testing.T) {
	mock := mockAnthropicServer("claude-3-opus-20240229", 5000, 2000)
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.anthropic.com")

	proxyAddr, err := st.RegisterContainer("anthropic-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.anthropic.com:%d/v1/messages", port)

	resp, err := client.Post(apiURL, "application/json",
		strings.NewReader(`{"model":"claude-3-opus-20240229","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	spending := st.GetSpending("anthropic-c1")
	if spending <= 0 {
		t.Errorf("spending = %d, want > 0 for Anthropic API call", spending)
	}

	// Verify pricing is correct:
	// claude-3-opus: input 1500, output 7500 micro-cents/token
	// 5000 * 1500 + 2000 * 7500 = 7.5M + 15M = 22.5M micro-cents = 22 cents
	expected := int64((5000*1500 + 2000*7500) / 1_000_000)
	if spending != expected {
		t.Errorf("spending = %d, want %d (calculated from opus pricing)", spending, expected)
	}
	t.Logf("Anthropic spending: %d cents", spending)
}

func TestProxyE2E_ExistingSpendingPreloaded(t *testing.T) {
	mock := mockOpenAIServer("gpt-4", 10000, 5000) // ~60 cents per call
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	// Register with 90 cents already spent, budget of 100
	proxyAddr, err := st.RegisterContainer("preload-c1", 100, 90)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	initial := st.GetSpending("preload-c1")
	if initial != 90 {
		t.Fatalf("initial spending = %d, want 90", initial)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	// 1st request: spending 90 < 100 → allowed. After: ~150 cents
	resp, err := client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("1st request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("1st request: status = %d, want 200", resp.StatusCode)
	}

	afterFirst := st.GetSpending("preload-c1")
	if afterFirst <= 90 {
		t.Errorf("spending after request = %d, should be > 90", afterFirst)
	}

	// 2nd request: spending ~150 >= 100 → blocked
	resp, err = client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("2nd request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("2nd request: status = %d, want 429", resp.StatusCode)
	}
}

func TestProxyE2E_ResponseBodyPassedThrough(t *testing.T) {
	mock := mockOpenAIServer("gpt-4o", 100, 50)
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	proxyAddr, err := st.RegisterContainer("body-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	resp, err := client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Verify the mock response body was passed through to the client
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	if result["id"] != "chatcmpl-test" {
		t.Errorf("response id = %v, want chatcmpl-test", result["id"])
	}
	if result["model"] != "gpt-4o" {
		t.Errorf("response model = %v, want gpt-4o", result["model"])
	}
}

func TestProxyE2E_RequestHeadersForwarded(t *testing.T) {
	var receivedAuth string
	var receivedContentType string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model": "gpt-4o",
			"usage": map[string]int64{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	proxyAddr, err := st.RegisterContainer("headers-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)
	req, _ := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-test-key-123")
	req.Header.Set("Content-Type", "application/json")

	client := proxyClient(proxyAddr)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if receivedAuth != "Bearer sk-test-key-123" {
		t.Errorf("Authorization header = %q, want %q", receivedAuth, "Bearer sk-test-key-123")
	}
	if receivedContentType != "application/json" {
		t.Errorf("Content-Type header = %q, want %q", receivedContentType, "application/json")
	}
}

func TestProxyE2E_MultipleContainersIsolated(t *testing.T) {
	mock := mockOpenAIServer("gpt-4", 10000, 5000) // ~60 cents per call
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	addr1, err := st.RegisterContainer("iso-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer c1: %v", err)
	}
	addr2, err := st.RegisterContainer("iso-c2", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer c2: %v", err)
	}

	client1 := proxyClient(addr1)
	client2 := proxyClient(addr2)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	// Send 3 requests through c1
	for i := 0; i < 3; i++ {
		resp, err := client1.Post(apiURL, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("c1 request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	// Send 1 request through c2
	resp, err := client2.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("c2 request: %v", err)
	}
	resp.Body.Close()

	s1 := st.GetSpending("iso-c1")
	s2 := st.GetSpending("iso-c2")

	if s1 <= s2 {
		t.Errorf("c1 spending (%d) should be > c2 spending (%d), c1 had 3x requests", s1, s2)
	}
	if s2 <= 0 {
		t.Errorf("c2 spending = %d, want > 0", s2)
	}

	t.Logf("c1: %d cents (3 requests), c2: %d cents (1 request)", s1, s2)
}

func TestProxyE2E_UpstreamError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer mock.Close()
	port := mockPort(mock)

	st := NewSpendingTracker(nil)
	st.transport = localRedirectTransport("api.openai.com")

	proxyAddr, err := st.RegisterContainer("err-c1", 0, 0)
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}

	client := proxyClient(proxyAddr)
	apiURL := fmt.Sprintf("http://api.openai.com:%d/v1/chat/completions", port)

	resp, err := client.Post(apiURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	// Upstream error status should be passed through
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	// No spending should be tracked (no valid usage in error response)
	spending := st.GetSpending("err-c1")
	if spending != 0 {
		t.Errorf("spending = %d, want 0 for error response", spending)
	}
}

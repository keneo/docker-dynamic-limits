package ollama

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/testutil"
)

func newMockOllama(delay time.Duration, response map[string]interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		// Verify stream=false in request
		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]interface{}
		json.Unmarshal(body, &reqBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
}

func newTestQueue(ollamaURL string) (*Queue, *testutil.MockProxy, *testutil.MockStore, *events.Bus) {
	mp := testutil.NewMockProxy()
	ms := testutil.NewMockStore()
	bus := events.NewBus()
	cfg := Config{
		OllamaURL:      ollamaURL,
		AllowedModels:  []string{"llama3.2:3b", "qwen3:8b"},
		MaxQueueSize:   5,
		RequestTimeout: 10 * time.Second,
		DefaultBid:     0,
	}
	q := NewQueue(cfg, mp, ms, bus)
	return q, mp, ms, bus
}

func makeRequest(t *testing.T, q *Queue, containerID, model, path string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	w := httptest.NewRecorder()
	q.HandleRequest(containerID, w, req)
	return w
}

func TestEnqueueDequeue(t *testing.T) {
	ollama := newMockOllama(0, map[string]interface{}{
		"model":   "llama3.2:3b",
		"message": map[string]string{"role": "assistant", "content": "Hello!"},
	})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if resp["model"] != "llama3.2:3b" {
		t.Errorf("model = %v, want llama3.2:3b", resp["model"])
	}
}

func TestBidPriority(t *testing.T) {
	// Use a slow Ollama to ensure both requests are enqueued before processing
	var processed []string
	var mu sync.Mutex
	callCount := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		mu.Lock()
		callCount++
		currentCall := callCount
		mu.Unlock()

		// First call is slow to give time to enqueue second request
		if currentCall == 1 {
			time.Sleep(100 * time.Millisecond)
		}

		mu.Lock()
		model := req["model"].(string)
		processed = append(processed, model)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"model": model})
	}))
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// First, enqueue a low-bid request that will start processing immediately
	q.SetBid("c1", 50)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	}()

	// Wait for c1 to start processing
	time.Sleep(20 * time.Millisecond)

	// Now enqueue two more while c1 is active - higher bid should be served next
	q.SetBid("c2", 100)
	q.SetBid("c3", 200)

	wg.Add(2)
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	}()
	time.Sleep(5 * time.Millisecond) // ensure c2 enqueues first
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c3", "qwen3:8b", "/api/chat")
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 3 {
		t.Fatalf("processed %d requests, want 3", len(processed))
	}
	// c1 is already active first, then c3 (bid=200) before c2 (bid=100)
	if processed[1] != "qwen3:8b" || processed[2] != "llama3.2:3b" {
		t.Errorf("processing order: %v; expected c3 (qwen3:8b) before c2 (llama3.2:3b)", processed)
	}
}

func TestFIFOForEqualBids(t *testing.T) {
	var processed []string
	var mu sync.Mutex
	callCount := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		mu.Lock()
		callCount++
		currentCall := callCount
		mu.Unlock()

		if currentCall == 1 {
			time.Sleep(100 * time.Millisecond)
		}

		mu.Lock()
		model := req["model"].(string)
		processed = append(processed, model)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"model": model})
	}))
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	q.SetBid("c1", 100)
	q.SetBid("c2", 100)
	q.SetBid("c3", 100)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	}()
	time.Sleep(20 * time.Millisecond)

	// Enqueue c2 then c3 with same bid - should be FIFO
	wg.Add(2)
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	}()
	time.Sleep(5 * time.Millisecond)
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c3", "qwen3:8b", "/api/chat")
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 3 {
		t.Fatalf("processed %d requests, want 3", len(processed))
	}
	// c2 enqueued before c3, same bid → c2 first (FIFO)
	if processed[1] != "llama3.2:3b" || processed[2] != "qwen3:8b" {
		t.Errorf("processing order: %v; expected FIFO for equal bids", processed)
	}
}

func TestOnePerContainer(t *testing.T) {
	// Slow Ollama so the first request stays active
	ollama := newMockOllama(500*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// First request - will block
	go makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	// Second request from same container should be rejected
	w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
}

func TestQueueTimeout(t *testing.T) {
	// Very slow Ollama
	ollama := newMockOllama(5*time.Second, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// Block the queue with a slow request
	go makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	// Second request with short queue_timeout
	body := map[string]interface{}{
		"model":         "llama3.2:3b",
		"messages":      []map[string]string{{"role": "user", "content": "Hello"}},
		"queue_timeout": 0.1, // 100ms
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(data))
	w := httptest.NewRecorder()
	q.HandleRequest("c2", w, req)

	if w.Code != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408; body = %s", w.Code, w.Body.String())
	}
}

func TestModelNotAllowed(t *testing.T) {
	ollama := newMockOllama(0, map[string]interface{}{})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	w := makeRequest(t, q, "c1", "unknown-model", "/api/chat")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestSpendingPreCheck(t *testing.T) {
	ollama := newMockOllama(0, map[string]interface{}{})
	defer ollama.Close()

	q, mp, ms, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// Set a spending limit and exceed it
	ms.SetLimit("c1", "spending", 1000)
	mp.SetSpending("c1", 1000)

	w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402; body = %s", w.Code, w.Body.String())
	}
}

func TestWallClockBilling(t *testing.T) {
	sleepDuration := 100 * time.Millisecond
	ollama := newMockOllama(sleepDuration, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, mp, _, bus := newTestQueue(ollama.URL)
	defer q.Stop()

	q.SetBid("c1", 1000) // 1000 milli-cents per wall-second

	// Subscribe for dequeue events
	sub := bus.Subscribe(events.Filter{Types: []events.EventType{events.OllamaDequeue}})
	defer bus.Unsubscribe(sub)

	w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Check spending was added
	spent := mp.GetSpending("c1")
	if spent < 100 { // at least 100ms * 1000 mc/s = 100 milli-cents
		t.Errorf("spending = %d, want >= 100 milli-cents", spent)
	}

	// Check dequeue event
	select {
	case evt := <-sub.C:
		var data events.OllamaDequeueData
		json.Unmarshal(evt.Data, &data)
		if data.WallSeconds < 0.1 {
			t.Errorf("wall_seconds = %f, want >= 0.1", data.WallSeconds)
		}
		if data.Cost < 100 {
			t.Errorf("cost = %d, want >= 100", data.Cost)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for dequeue event")
	}
}

func TestBidUpdate(t *testing.T) {
	// Slow Ollama to keep queue populated
	ollama := newMockOllama(200*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	q.SetBid("c1", 50)
	q.SetBid("c2", 50)

	// Block with c1 active
	go makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	// Enqueue c2
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	}()
	time.Sleep(10 * time.Millisecond)

	// Update c2's bid
	q.SetBid("c2", 200)

	bid := q.GetBid("c2")
	if bid != 200 {
		t.Errorf("bid = %d, want 200", bid)
	}

	wg.Wait()
}

func TestRemoveContainer(t *testing.T) {
	ollama := newMockOllama(500*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	q.SetBid("c1", 100)

	// Block with another container active
	go makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	// Enqueue c1, it will be pending
	done := make(chan int)
	go func() {
		w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
		done <- w.Code
	}()
	time.Sleep(20 * time.Millisecond)

	// Remove c1 - should cancel pending request
	q.RemoveContainer("c1")

	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502 (cancelled)", code)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for cancelled request")
	}

	bid := q.GetBid("c1")
	if bid != 0 {
		t.Errorf("bid = %d, want 0 after removal", bid)
	}
}

func TestQueueCancel(t *testing.T) {
	ollama := newMockOllama(500*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// Block with another container
	go makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	// Enqueue c1 pending
	done := make(chan int)
	go func() {
		w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
		done <- w.Code
	}()
	time.Sleep(20 * time.Millisecond)

	ok := q.CancelEntry("c1")
	if !ok {
		t.Error("CancelEntry returned false, expected true")
	}

	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502 (cancelled)", code)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for cancelled request")
	}
}

func TestStreamForcedFalse(t *testing.T) {
	var receivedStream interface{}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)
		receivedStream = req["stream"]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"model": "llama3.2:3b"})
	}))
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// Send with stream: true, it should be forced to false
	body := map[string]interface{}{
		"model":    "llama3.2:3b",
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
		"stream":   true,
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(data))
	w := httptest.NewRecorder()
	q.HandleRequest("c1", w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if receivedStream != false {
		t.Errorf("stream = %v, want false", receivedStream)
	}
}

func TestQueueVisibility(t *testing.T) {
	ollama := newMockOllama(500*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	// Start active request
	go makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	// Enqueue a pending request
	go makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)

	status := q.QueueStatus()
	if status.Active == nil {
		t.Fatal("expected active entry")
	}
	if status.Active.ContainerID != "c1" {
		t.Errorf("active container = %s, want c1", status.Active.ContainerID)
	}
	if len(status.Pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(status.Pending))
	}
	if status.Pending[0].ContainerID != "c2" {
		t.Errorf("pending container = %s, want c2", status.Pending[0].ContainerID)
	}
}

func TestMaxQueueSize(t *testing.T) {
	ollama := newMockOllama(1*time.Second, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	q.cfg.MaxQueueSize = 2
	defer q.Stop()

	// Fill queue: 1 active + 2 pending = max
	go makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	time.Sleep(20 * time.Millisecond)
	go makeRequest(t, q, "c2", "llama3.2:3b", "/api/chat")
	time.Sleep(10 * time.Millisecond)
	go makeRequest(t, q, "c3", "qwen3:8b", "/api/chat")
	time.Sleep(10 * time.Millisecond)

	// Queue should be full now
	w := makeRequest(t, q, "c4", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
}

func TestNonInferencePath(t *testing.T) {
	ollama := newMockOllama(0, map[string]interface{}{})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	body := map[string]interface{}{"model": "llama3.2:3b"}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/tags", bytes.NewReader(data))
	w := httptest.NewRecorder()
	q.HandleRequest("c1", w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ollama := newMockOllama(0, map[string]interface{}{})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
	w := httptest.NewRecorder()
	q.HandleRequest("c1", w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestAllowedModels(t *testing.T) {
	q, _, _, _ := newTestQueue("http://localhost:11434")
	defer q.Stop()

	models := q.AllowedModels()
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0] != "llama3.2:3b" || models[1] != "qwen3:8b" {
		t.Errorf("models = %v, want [llama3.2:3b qwen3:8b]", models)
	}
}

func TestOllamaHost(t *testing.T) {
	q, _, _, _ := newTestQueue("http://192.168.1.100:11434")
	defer q.Stop()

	host := q.OllamaHost()
	if host != "192.168.1.100" {
		t.Errorf("host = %q, want %q", host, "192.168.1.100")
	}
}

func TestAllModelsAllowedWhenEmpty(t *testing.T) {
	mp := testutil.NewMockProxy()
	ms := testutil.NewMockStore()
	bus := events.NewBus()

	ollama := newMockOllama(0, map[string]interface{}{"model": "any-model"})
	defer ollama.Close()

	cfg := Config{
		OllamaURL:      ollama.URL,
		AllowedModels:  nil, // empty = allow all
		MaxQueueSize:   5,
		RequestTimeout: 10 * time.Second,
	}
	q := NewQueue(cfg, mp, ms, bus)
	defer q.Stop()

	w := makeRequest(t, q, "c1", "any-model", "/api/chat")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (all models allowed); body = %s", w.Code, w.Body.String())
	}
}

func TestGeneratePath(t *testing.T) {
	ollama := newMockOllama(0, map[string]interface{}{"model": "llama3.2:3b", "response": "hello"})
	defer ollama.Close()

	q, _, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	body := map[string]interface{}{
		"model":  "llama3.2:3b",
		"prompt": "Hello",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(data))
	w := httptest.NewRecorder()
	q.HandleRequest("c1", w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
}

func TestOllamaBillingSleepSubtraction(t *testing.T) {
	ollama := newMockOllama(100*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	q, mp, _, _ := newTestQueue(ollama.URL)
	defer q.Stop()

	q.SetBid("c1", 1000) // 1000 milli-cents per wall-second

	// Inject a fake sleep event with a wide window that will definitely
	// overlap the request processing time. Sleep range = [now, now+1s].
	// The request takes ~100ms, so the entire request falls within the sleep.
	now := time.Now()
	q.recordSleep(now.Add(1*time.Second), 1*time.Second)

	w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// The entire ~100ms request falls within the 1s sleep window,
	// so wallSeconds should be reduced to ~0 and cost should be 0.
	spent := mp.GetSpending("c1")
	if spent != 0 {
		t.Errorf("spending = %d, want 0 (entire request during sleep)", spent)
	}
}

func TestSleepDuring(t *testing.T) {
	q, _, _, _ := newTestQueue("http://localhost:11434")
	defer q.Stop()

	base := time.Now()

	// Record a sleep event: at base+10s, duration 5s (so sleep was from base+5s to base+10s)
	q.recordSleep(base.Add(10*time.Second), 5*time.Second)

	// Query overlapping range
	dur := q.sleepDuring(base.Add(7*time.Second), base.Add(12*time.Second))
	// Sleep is [base+5s, base+10s], query is [base+7s, base+12s] → overlap is [base+7s, base+10s] = 3s
	if dur != 3*time.Second {
		t.Errorf("sleepDuring = %v, want 3s", dur)
	}

	// Query non-overlapping range
	dur = q.sleepDuring(base.Add(11*time.Second), base.Add(15*time.Second))
	if dur != 0 {
		t.Errorf("sleepDuring = %v, want 0", dur)
	}

	// Query fully containing the sleep
	dur = q.sleepDuring(base, base.Add(20*time.Second))
	if dur != 5*time.Second {
		t.Errorf("sleepDuring = %v, want 5s", dur)
	}
}

func TestGetConfig(t *testing.T) {
	q, _, _, _ := newTestQueue("http://192.168.1.100:11434")
	defer q.Stop()

	cfg := q.GetConfig()
	if cfg.OllamaURL != "http://192.168.1.100:11434" {
		t.Errorf("OllamaURL = %q, want http://192.168.1.100:11434", cfg.OllamaURL)
	}
	if len(cfg.AllowedModels) != 2 {
		t.Fatalf("AllowedModels len = %d, want 2", len(cfg.AllowedModels))
	}
	if cfg.MaxQueueSize != 5 {
		t.Errorf("MaxQueueSize = %d, want 5", cfg.MaxQueueSize)
	}

	// Verify returned slice is a copy
	cfg.AllowedModels[0] = "modified"
	orig := q.GetConfig()
	if orig.AllowedModels[0] == "modified" {
		t.Error("GetConfig should return a copy, not a reference")
	}
}

func TestSetAllowedModels(t *testing.T) {
	q, _, _, _ := newTestQueue("http://localhost:11434")
	defer q.Stop()

	q.SetAllowedModels([]string{"new-model"})
	cfg := q.GetConfig()
	if len(cfg.AllowedModels) != 1 || cfg.AllowedModels[0] != "new-model" {
		t.Errorf("AllowedModels = %v, want [new-model]", cfg.AllowedModels)
	}
}

func TestSetMaxQueueSize(t *testing.T) {
	q, _, _, _ := newTestQueue("http://localhost:11434")
	defer q.Stop()

	q.SetMaxQueueSize(100)
	cfg := q.GetConfig()
	if cfg.MaxQueueSize != 100 {
		t.Errorf("MaxQueueSize = %d, want 100", cfg.MaxQueueSize)
	}
}

func TestSetRequestTimeout(t *testing.T) {
	q, _, _, _ := newTestQueue("http://localhost:11434")
	defer q.Stop()

	q.SetRequestTimeout(5 * time.Minute)
	cfg := q.GetConfig()
	if cfg.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v, want 5m", cfg.RequestTimeout)
	}
	// Also verify the client timeout is updated
	if q.client.Timeout != 5*time.Minute {
		t.Errorf("client.Timeout = %v, want 5m", q.client.Timeout)
	}
}

func TestSetDefaultBidMethod(t *testing.T) {
	q, _, _, _ := newTestQueue("http://localhost:11434")
	defer q.Stop()

	q.SetDefaultBid(500)
	cfg := q.GetConfig()
	if cfg.DefaultBid != 500 {
		t.Errorf("DefaultBid = %d, want 500", cfg.DefaultBid)
	}
}

func TestDefaultBid(t *testing.T) {
	ollama := newMockOllama(50*time.Millisecond, map[string]interface{}{"model": "llama3.2:3b"})
	defer ollama.Close()

	mp := testutil.NewMockProxy()
	ms := testutil.NewMockStore()
	bus := events.NewBus()
	cfg := Config{
		OllamaURL:      ollama.URL,
		AllowedModels:  []string{"llama3.2:3b"},
		MaxQueueSize:   5,
		RequestTimeout: 10 * time.Second,
		DefaultBid:     500,
	}
	q := NewQueue(cfg, mp, ms, bus)
	defer q.Stop()

	w := makeRequest(t, q, "c1", "llama3.2:3b", "/api/chat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// With DefaultBid=500 and ~50ms wall time, cost should be ~25 milli-cents
	spent := mp.GetSpending("c1")
	if spent == 0 {
		t.Error("spending should be > 0 with default bid")
	}
	fmt.Printf("  DefaultBid test: spent=%d milli-cents (expected ~25)\n", spent)
}

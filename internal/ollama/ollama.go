package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/proxy"
	"github.com/keneo/docker-dynamic-limits/internal/store"
)

// Config holds configuration for the Ollama inference queue.
type Config struct {
	OllamaURL      string        // e.g. http://192.168.1.100:11434
	AllowedModels  []string      // e.g. ["llama3.2:3b", "qwen3:8b"]
	MaxQueueSize   int           // default 50
	RequestTimeout time.Duration // timeout for Ollama HTTP call, default 120s
	DefaultBid     int64         // default bid in milli-cents per wall-second, default 0
}

// result is sent back to the waiting HTTP handler.
type result struct {
	statusCode int
	header     http.Header
	body       []byte
	err        error
}

// entry represents a queued inference request.
type entry struct {
	containerID string
	model       string
	path        string // original path from request (e.g. /api/chat)
	body        []byte // request body (queue_timeout stripped, stream: false forced)
	bid         int64
	enqueuedAt  time.Time
	timeout     time.Duration // 0 = unlimited
	resultCh    chan *result   // buffered(1)
	cancelled   int32         // atomic flag
	ctx         context.Context
	cancel      context.CancelFunc
}

// QueueStatusResponse is the JSON response for GET /ollama/queue.
type QueueStatusResponse struct {
	Active  *QueueEntry  `json:"active"`
	Pending []QueueEntry `json:"pending"`
	Size    int          `json:"size"`
}

// QueueEntry is a single entry in the queue status response.
type QueueEntry struct {
	ContainerID string    `json:"container_id"`
	Model       string    `json:"model"`
	Bid         int64     `json:"bid"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// sleepEvent records a detected system sleep period.
type sleepEvent struct {
	at       time.Time
	duration time.Duration
}

// Queue manages the Ollama inference queue with bid-based priority.
// ActivityRecorder is called after each Ollama request to record proxy activity.
type ActivityRecorder func(containerID string, act proxy.ProxyActivity)

type Queue struct {
	cfg    Config
	proxy  proxy.SpendingProxy
	store  store.DataStore
	bus    *events.Bus
	client *http.Client

	mu      sync.Mutex
	bids    map[string]int64 // containerID → standing bid (milli-cents per wall-second)
	entries []*entry         // sorted: highest bid first, FIFO for equal bids
	active  *entry           // currently being processed by Ollama

	sleepMu     sync.Mutex
	sleepEvents []sleepEvent

	onActivity ActivityRecorder // optional callback for proxy activity recording

	wake chan struct{} // buffered(1), signals processor
	done chan struct{} // shutdown
}

// NewQueue creates a new inference queue and starts the processor goroutine.
func NewQueue(cfg Config, px proxy.SpendingProxy, st store.DataStore, bus *events.Bus) *Queue {
	if cfg.MaxQueueSize <= 0 {
		cfg.MaxQueueSize = 50
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 120 * time.Second
	}

	q := &Queue{
		cfg:    cfg,
		proxy:  px,
		store:  st,
		bus:    bus,
		client: &http.Client{Timeout: cfg.RequestTimeout},
		bids:   make(map[string]int64),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go q.processLoop()
	go q.sleepDetectorLoop()
	return q
}

// Stop shuts down the queue processor and drains pending entries with errors.
func (q *Queue) Stop() {
	close(q.done)
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.entries {
		if atomic.CompareAndSwapInt32(&e.cancelled, 0, 1) {
			e.cancel()
			e.resultCh <- &result{err: fmt.Errorf("queue shutting down")}
		}
	}
	q.entries = nil
	if q.active != nil {
		q.active.cancel()
	}
}

// HandleRequest is the main inference handler, called from the proxy when host matches Ollama.
func (q *Queue) HandleRequest(containerID string, w http.ResponseWriter, r *http.Request) {
	// Only allow POST to inference paths
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Path
	if path != "/api/chat" && path != "/api/generate" {
		http.Error(w, `{"error":"only /api/chat and /api/generate are supported"}`, http.StatusBadRequest)
		return
	}

	// Read and parse body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &bodyMap); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	// Validate model
	modelName, _ := bodyMap["model"].(string)
	if modelName == "" {
		http.Error(w, `{"error":"model field required"}`, http.StatusBadRequest)
		return
	}
	if !q.isModelAllowed(modelName) {
		http.Error(w, fmt.Sprintf(`{"error":"model %q not allowed"}`, modelName), http.StatusBadRequest)
		return
	}

	// Pre-check spending
	if q.store != nil && q.proxy != nil {
		limit, _ := q.store.GetLimit(containerID, "spending")
		spent := q.proxy.GetSpending(containerID)
		if limit > 0 && spent >= limit {
			http.Error(w, `{"error":"spending budget exceeded"}`, http.StatusPaymentRequired)
			return
		}
	}

	// Check one-per-container (pending or active)
	q.mu.Lock()
	if q.active != nil && q.active.containerID == containerID {
		q.mu.Unlock()
		http.Error(w, `{"error":"container already has an active request"}`, http.StatusConflict)
		return
	}
	for _, e := range q.entries {
		if e.containerID == containerID && atomic.LoadInt32(&e.cancelled) == 0 {
			q.mu.Unlock()
			http.Error(w, `{"error":"container already has a pending request"}`, http.StatusConflict)
			return
		}
	}
	q.mu.Unlock()

	// Extract and strip queue_timeout
	var queueTimeout time.Duration
	if qt, ok := bodyMap["queue_timeout"]; ok {
		switch v := qt.(type) {
		case float64:
			queueTimeout = time.Duration(v * float64(time.Second))
		case string:
			queueTimeout, _ = time.ParseDuration(v)
		}
		delete(bodyMap, "queue_timeout")
	}

	// Force stream: false
	bodyMap["stream"] = false

	// Re-marshal the body
	cleanBody, err := json.Marshal(bodyMap)
	if err != nil {
		http.Error(w, `{"error":"failed to marshal request"}`, http.StatusInternalServerError)
		return
	}

	// Get current bid
	q.mu.Lock()
	bid := q.bids[containerID]
	if bid == 0 {
		bid = q.cfg.DefaultBid
	}

	// Check queue size
	if len(q.entries) >= q.cfg.MaxQueueSize {
		q.mu.Unlock()
		http.Error(w, `{"error":"queue is full"}`, http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	e := &entry{
		containerID: containerID,
		model:       modelName,
		path:        path,
		body:        cleanBody,
		bid:         bid,
		enqueuedAt:  time.Now(),
		timeout:     queueTimeout,
		resultCh:    make(chan *result, 1),
		ctx:         ctx,
		cancel:      cancel,
	}

	q.entries = append(q.entries, e)
	q.sortEntries()
	q.mu.Unlock()

	// Emit enqueue event
	if q.bus != nil {
		q.bus.PublishData(events.OllamaEnqueue, containerID, events.OllamaEnqueueData{
			Model: modelName,
			Bid:   bid,
		})
	}

	// Signal processor
	select {
	case q.wake <- struct{}{}:
	default:
	}

	// Wait for result
	var timeoutCh <-chan time.Time
	if queueTimeout > 0 {
		timer := time.NewTimer(queueTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case res := <-e.resultCh:
		if res.err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, res.err.Error()), http.StatusBadGateway)
			return
		}
		for k, vv := range res.header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.statusCode)
		w.Write(res.body)

	case <-timeoutCh:
		atomic.StoreInt32(&e.cancelled, 1)
		e.cancel()
		q.removeEntry(e)
		if q.bus != nil {
			q.bus.PublishData(events.OllamaCancel, containerID, events.OllamaCancelData{Reason: "timeout"})
		}
		http.Error(w, `{"error":"queue timeout"}`, http.StatusRequestTimeout)

	case <-r.Context().Done():
		atomic.StoreInt32(&e.cancelled, 1)
		e.cancel()
		q.removeEntry(e)
		if q.bus != nil {
			q.bus.PublishData(events.OllamaCancel, containerID, events.OllamaCancelData{Reason: "cancelled"})
		}
	}
}

// SetBid sets the standing bid for a container and re-sorts pending entries.
func (q *Queue) SetBid(containerID string, bid int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.bids[containerID] = bid
	// Update bid on any pending entry for this container
	for _, e := range q.entries {
		if e.containerID == containerID && atomic.LoadInt32(&e.cancelled) == 0 {
			e.bid = bid
		}
	}
	q.sortEntries()
	if q.bus != nil {
		q.bus.PublishData(events.OllamaBidChange, containerID, events.OllamaBidChangeData{Bid: bid})
	}
}

// GetBid returns the current standing bid for a container.
func (q *Queue) GetBid(containerID string) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bids[containerID]
}

// CancelEntry cancels a pending entry for a container.
func (q *Queue) CancelEntry(containerID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, e := range q.entries {
		if e.containerID == containerID && atomic.CompareAndSwapInt32(&e.cancelled, 0, 1) {
			e.cancel()
			e.resultCh <- &result{err: fmt.Errorf("cancelled")}
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			if q.bus != nil {
				q.bus.PublishData(events.OllamaCancel, containerID, events.OllamaCancelData{Reason: "cancelled"})
			}
			return true
		}
	}
	return false
}

// RemoveContainer cancels pending entries, removes bid, discards active if being processed.
func (q *Queue) RemoveContainer(containerID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.bids, containerID)

	// Cancel pending entries
	remaining := q.entries[:0]
	for _, e := range q.entries {
		if e.containerID == containerID {
			if atomic.CompareAndSwapInt32(&e.cancelled, 0, 1) {
				e.cancel()
				e.resultCh <- &result{err: fmt.Errorf("container removed")}
				if q.bus != nil {
					q.bus.PublishData(events.OllamaCancel, containerID, events.OllamaCancelData{Reason: "removed"})
				}
			}
		} else {
			remaining = append(remaining, e)
		}
	}
	q.entries = remaining

	// Cancel active request if it's for this container
	if q.active != nil && q.active.containerID == containerID {
		q.active.cancel()
	}
}

// QueueStatus returns the current queue state.
func (q *Queue) QueueStatus() QueueStatusResponse {
	q.mu.Lock()
	defer q.mu.Unlock()

	resp := QueueStatusResponse{
		Size: len(q.entries),
	}

	if q.active != nil {
		resp.Active = &QueueEntry{
			ContainerID: q.active.containerID,
			Model:       q.active.model,
			Bid:         q.active.bid,
			EnqueuedAt:  q.active.enqueuedAt,
		}
	}

	for _, e := range q.entries {
		if atomic.LoadInt32(&e.cancelled) == 0 {
			resp.Pending = append(resp.Pending, QueueEntry{
				ContainerID: e.containerID,
				Model:       e.model,
				Bid:         e.bid,
				EnqueuedAt:  e.enqueuedAt,
			})
		}
	}

	return resp
}

// AllowedModels returns the configured model allowlist.
func (q *Queue) AllowedModels() []string {
	return q.cfg.AllowedModels
}

// GetConfig returns a copy of the current queue configuration.
func (q *Queue) GetConfig() Config {
	q.mu.Lock()
	defer q.mu.Unlock()
	cfg := q.cfg
	cfg.AllowedModels = make([]string, len(q.cfg.AllowedModels))
	copy(cfg.AllowedModels, q.cfg.AllowedModels)
	return cfg
}

// SetAllowedModels updates the model allowlist at runtime.
func (q *Queue) SetAllowedModels(models []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cfg.AllowedModels = make([]string, len(models))
	copy(q.cfg.AllowedModels, models)
}

// SetMaxQueueSize updates the maximum queue size at runtime.
func (q *Queue) SetMaxQueueSize(size int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cfg.MaxQueueSize = size
}

// SetRequestTimeout updates the Ollama request timeout at runtime.
func (q *Queue) SetRequestTimeout(d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cfg.RequestTimeout = d
	q.client.Timeout = d
}

// SetDefaultBid updates the default bid at runtime.
func (q *Queue) SetDefaultBid(bid int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cfg.DefaultBid = bid
}

// SetActivityRecorder sets a callback that is called after each Ollama request.
func (q *Queue) SetActivityRecorder(fn ActivityRecorder) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onActivity = fn
}

// SetOllamaURL updates the Ollama server URL at runtime.
func (q *Queue) SetOllamaURL(u string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cfg.OllamaURL = u
}

// OllamaHost returns the host portion of the configured Ollama URL.
func (q *Queue) OllamaHost() string {
	u, err := url.Parse(q.cfg.OllamaURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// processLoop is the queue processor goroutine.
func (q *Queue) processLoop() {
	for {
		select {
		case <-q.done:
			return
		case <-q.wake:
		}

		for {
			e := q.popNext()
			if e == nil {
				break
			}

			q.mu.Lock()
			q.active = e
			q.mu.Unlock()

			q.processEntry(e)

			q.mu.Lock()
			q.active = nil
			q.mu.Unlock()
		}
	}
}

func (q *Queue) popNext() *entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.entries) > 0 {
		e := q.entries[0]
		q.entries = q.entries[1:]
		if atomic.LoadInt32(&e.cancelled) == 0 {
			return e
		}
	}
	return nil
}

func (q *Queue) processEntry(e *entry) {
	q.mu.Lock()
	targetURL := q.cfg.OllamaURL + e.path
	q.mu.Unlock()

	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, targetURL, nil)
	if err != nil {
		e.resultCh <- &result{err: fmt.Errorf("create request: %w", err)}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(jsonReader(e.body))
	req.ContentLength = int64(len(e.body))

	start := time.Now().Round(0) // wall clock only (monotonic clock doesn't advance during sleep)
	resp, err := q.client.Do(req)
	end := time.Now().Round(0)
	wallSeconds := end.Sub(start).Seconds()
	sleepDur := q.sleepDuring(start, end)
	wallSeconds -= sleepDur.Seconds()
	if wallSeconds < 0 {
		wallSeconds = 0
	}

	if err != nil {
		e.resultCh <- &result{err: fmt.Errorf("ollama request: %w", err)}
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.resultCh <- &result{err: fmt.Errorf("read response: %w", err)}
		return
	}

	// Wall-clock billing
	costMilliCents := int64(wallSeconds * float64(e.bid))
	if costMilliCents > 0 && q.proxy != nil {
		q.proxy.AddSpending(e.containerID, costMilliCents)
	}

	// Emit dequeue event
	if q.bus != nil {
		q.bus.PublishData(events.OllamaDequeue, e.containerID, events.OllamaDequeueData{
			Model:       e.model,
			WallSeconds: wallSeconds,
			Cost:        costMilliCents,
		})
	}

	log.Printf("[ollama] container %s: %s completed in %.1fs, cost=%d milli-cents (bid=%d)",
		e.containerID, e.model, wallSeconds, costMilliCents, e.bid)

	// Record activity
	q.mu.Lock()
	recorder := q.onActivity
	q.mu.Unlock()
	if recorder != nil {
		reqBody := string(e.body)
		if len(reqBody) > 4096 {
			reqBody = reqBody[:4096] + "..."
		}
		respBody := string(body)
		if len(respBody) > 4096 {
			respBody = respBody[:4096] + "..."
		}
		recorder(e.containerID, proxy.ProxyActivity{
			Timestamp:    end,
			Host:         "ollama",
			Path:         e.path,
			Method:       "POST",
			RequestBody:  reqBody,
			StatusCode:   resp.StatusCode,
			ResponseBody: respBody,
			Model:        e.model,
			CostMicro:    costMilliCents * 1_000, // milli-cents → micro-cents
			DurationMs:   int64(wallSeconds * 1000),
		})
	}

	e.resultCh <- &result{
		statusCode: resp.StatusCode,
		header:     resp.Header,
		body:       body,
	}
}

func (q *Queue) sleepDetectorLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := time.Now().Round(0) // strip monotonic reading for wall-clock comparison
	for {
		select {
		case <-q.done:
			return
		case <-ticker.C:
			now := time.Now().Round(0) // strip monotonic reading
			elapsed := now.Sub(last)
			last = now
			if elapsed > 5*time.Second {
				q.recordSleep(now, elapsed-time.Second)
				// Reset the ticker — after VM suspend/resume the internal
				// monotonic-based timer can get stuck.
				ticker.Reset(time.Second)
			}
		}
	}
}

func (q *Queue) recordSleep(at time.Time, duration time.Duration) {
	q.sleepMu.Lock()
	defer q.sleepMu.Unlock()
	q.sleepEvents = append(q.sleepEvents, sleepEvent{at: at, duration: duration})
}

// sleepDuring returns the total sleep duration overlapping with the [start, end) range.
func (q *Queue) sleepDuring(start, end time.Time) time.Duration {
	q.sleepMu.Lock()
	defer q.sleepMu.Unlock()
	var total time.Duration
	for _, se := range q.sleepEvents {
		sleepStart := se.at.Add(-se.duration)
		sleepEnd := se.at
		// Overlap with [start, end)
		overlapStart := sleepStart
		if start.After(overlapStart) {
			overlapStart = start
		}
		overlapEnd := sleepEnd
		if end.Before(overlapEnd) {
			overlapEnd = end
		}
		if overlapEnd.After(overlapStart) {
			total += overlapEnd.Sub(overlapStart)
		}
	}
	return total
}

func (q *Queue) removeEntry(target *entry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, e := range q.entries {
		if e == target {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			return
		}
	}
}

func (q *Queue) sortEntries() {
	sort.SliceStable(q.entries, func(i, j int) bool {
		return q.entries[i].bid > q.entries[j].bid
	})
}

func (q *Queue) isModelAllowed(model string) bool {
	if len(q.cfg.AllowedModels) == 0 {
		return true // no allowlist configured, allow all
	}
	for _, m := range q.cfg.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func jsonReader(data []byte) io.Reader {
	return &byteReader{data: data}
}

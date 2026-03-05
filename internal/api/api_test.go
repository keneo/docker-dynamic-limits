package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/testutil"
)

func newTestServer() (*httptest.Server, *testutil.MockStore, *testutil.MockDocker, *testutil.MockEnforcement, *testutil.MockProxy, *events.Bus) {
	ms := testutil.NewMockStore()
	md := testutil.NewMockDocker()
	me := testutil.NewMockEnforcement()
	mp := testutil.NewMockProxy()
	bus := events.NewBus()
	srv := NewServer(ms, md, me, mp, bus, nil)
	ts := httptest.NewServer(srv.Handler())
	return ts, ms, md, me, mp, bus
}

func newReadOnlyTestServer() (*httptest.Server, *testutil.MockStore, *testutil.MockDocker, *Server, *events.Bus) {
	ms := testutil.NewMockStore()
	md := testutil.NewMockDocker()
	me := testutil.NewMockEnforcement()
	mp := testutil.NewMockProxy()
	bus := events.NewBus()
	srv := NewServer(ms, md, me, mp, bus, nil)
	ts := httptest.NewServer(srv.ReadOnlyHandler())
	return ts, ms, md, srv, bus
}

func TestListContainersEmpty(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/containers")
	if err != nil {
		t.Fatalf("GET /containers: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var result []model.ContainerStatus
	json.NewDecoder(resp.Body).Decode(&result)
	if result != nil && len(result) != 0 {
		t.Errorf("expected empty list, got %d items", len(result))
	}
}

func TestRegisterContainer(t *testing.T) {
	ts, _, md, me, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	md.AddContainer(dockerID, "test-container", true)

	body, _ := json.Marshal(map[string]string{"container_id": dockerID})
	resp, err := http.Post(ts.URL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var result model.Container
	json.NewDecoder(resp.Body).Decode(&result)
	if result.ID != dockerID[:12] {
		t.Errorf("ID = %q, want %q", result.ID, dockerID[:12])
	}
	if result.Name != "test-container" {
		t.Errorf("Name = %q, want %q", result.Name, "test-container")
	}

	// Verify enforcement was started
	if !me.WasStarted(result.ID) {
		t.Error("enforcement was not started for registered container")
	}
}

func TestRegisterContainerMissingID(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{})
	resp, err := http.Post(ts.URL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRegisterContainerNotFound(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"container_id": "nonexistent"})
	resp, err := http.Post(ts.URL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetContainerInfo(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(c.ID, model.LimitCPU, 3600)
	ms.SetUsage(c.ID, model.LimitCPU, 100)

	resp, err := http.Get(ts.URL + "/containers/" + c.ID)
	if err != nil {
		t.Fatalf("GET /containers/{id}: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var status model.ContainerStatus
	json.NewDecoder(resp.Body).Decode(&status)
	if status.Container.ID != c.ID {
		t.Errorf("ID = %q, want %q", status.Container.ID, c.ID)
	}
	if status.Limits[model.LimitCPU] != 3600 {
		t.Errorf("CPU limit = %d, want 3600", status.Limits[model.LimitCPU])
	}
	if status.Usage[model.LimitCPU] != 100 {
		t.Errorf("CPU usage = %d, want 100", status.Usage[model.LimitCPU])
	}
}

func TestGetContainerNotFound(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/containers/nonexistent")
	if err != nil {
		t.Fatalf("GET /containers/nonexistent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteContainer(t *testing.T) {
	ts, ms, _, me, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/containers/"+c.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /containers/{id}: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Verify enforcement was stopped
	if !me.WasStopped(c.ID) {
		t.Error("enforcement was not stopped for deleted container")
	}
}

func TestSetLimitSet(t *testing.T) {
	ts, ms, _, me, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")

	body, _ := json.Marshal(map[string]interface{}{
		"type":      "cpu",
		"value":     3600,
		"operation": "set",
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/containers/"+c.ID+"/limits", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT limits: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Verify limit was set
	lim, _ := ms.GetLimit(c.ID, model.LimitCPU)
	if lim != 3600 {
		t.Errorf("limit = %d, want 3600", lim)
	}

	// Verify NotifyLimitChanged was called
	if !me.WasNotified() {
		t.Error("NotifyLimitChanged was not called")
	}
}

func TestSetLimitIncrease(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(c.ID, model.LimitCPU, 100)

	body, _ := json.Marshal(map[string]interface{}{
		"type":      "cpu",
		"value":     50,
		"operation": "increase",
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/containers/"+c.ID+"/limits", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()

	lim, _ := ms.GetLimit(c.ID, model.LimitCPU)
	if lim != 150 {
		t.Errorf("limit = %d, want 150", lim)
	}
}

func TestSetLimitDecrease(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(c.ID, model.LimitCPU, 100)

	body, _ := json.Marshal(map[string]interface{}{
		"type":      "cpu",
		"value":     150,
		"operation": "decrease",
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/containers/"+c.ID+"/limits", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()

	lim, _ := ms.GetLimit(c.ID, model.LimitCPU)
	if lim != 0 {
		t.Errorf("limit = %d, want 0 (floor)", lim)
	}
}

func TestSetSpendingLimit(t *testing.T) {
	ts, ms, _, _, mp, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")

	body, _ := json.Marshal(map[string]interface{}{
		"type":      "spending",
		"value":     1000,
		"operation": "set",
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/containers/"+c.ID+"/limits", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if budget := mp.GetBudget(c.ID); budget != 1000 {
		t.Errorf("proxy budget = %d, want 1000", budget)
	}
}

func TestGetLimits(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(c.ID, model.LimitCPU, 3600)
	ms.SetLimit(c.ID, model.LimitRAM, 1073741824)

	resp, err := http.Get(ts.URL + "/containers/" + c.ID + "/limits")
	if err != nil {
		t.Fatalf("GET limits: %v", err)
	}
	defer resp.Body.Close()

	var limits map[string]int64
	json.NewDecoder(resp.Body).Decode(&limits)
	if limits["cpu"] != 3600 {
		t.Errorf("cpu = %d, want 3600", limits["cpu"])
	}
}

func TestGetUsage(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetUsage(c.ID, model.LimitCPU, 42)

	resp, err := http.Get(ts.URL + "/containers/" + c.ID + "/usage")
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()

	var usage map[string]int64
	json.NewDecoder(resp.Body).Decode(&usage)
	if usage["cpu"] != 42 {
		t.Errorf("cpu = %d, want 42", usage["cpu"])
	}
}

func TestSelfUsage(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetUsage(c.ID, model.LimitCPU, 10)
	ms.SetLimit(c.ID, model.LimitCPU, 100)

	resp, err := http.Get(ts.URL + "/usage?id=" + c.ID)
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestSelfUsageNoID(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSelfLimits(t *testing.T) {
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(c.ID, model.LimitCPU, 3600)

	resp, err := http.Get(ts.URL + "/limits?id=" + c.ID)
	if err != nil {
		t.Fatalf("GET /limits: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/containers", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /containers: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestReadOnlyHandlerAllowsGet(t *testing.T) {
	ts, ms, md, srv, _ := newReadOnlyTestServer()
	defer ts.Close()
	defer srv.Stop()

	dockerID := "abcdef123456789000"
	md.AddContainer(dockerID, "test", true)
	md.SetContainerIP(dockerID, "127.0.0.1")
	c, _ := ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(c.ID, model.LimitCPU, 3600)
	ms.SetUsage(c.ID, model.LimitCPU, 10)

	// Refresh IP map so the server knows about our container
	srv.refreshIPs()

	// GET /containers should succeed
	resp, err := http.Get(ts.URL + "/containers")
	if err != nil {
		t.Fatalf("GET /containers: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /containers: status = %d, want 200", resp.StatusCode)
	}

	// GET /usage should succeed (resolved by source IP)
	resp, err = http.Get(ts.URL + "/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /usage: status = %d, want 200", resp.StatusCode)
	}

	// GET /limits should succeed (resolved by source IP)
	resp, err = http.Get(ts.URL + "/limits")
	if err != nil {
		t.Fatalf("GET /limits: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /limits: status = %d, want 200", resp.StatusCode)
	}
}

func TestReadOnlyIPResolutionUnknownIP(t *testing.T) {
	ts, ms, md, srv, _ := newReadOnlyTestServer()
	defer ts.Close()
	defer srv.Stop()

	dockerID := "abcdef123456789000"
	md.AddContainer(dockerID, "test", true)
	md.SetContainerIP(dockerID, "10.0.0.99") // different from 127.0.0.1
	ms.RegisterContainer(dockerID, "test")

	srv.refreshIPs()

	// GET /usage from 127.0.0.1 should return 403 (unknown container)
	resp, err := http.Get(ts.URL + "/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /usage: status = %d, want 403", resp.StatusCode)
	}
}

func TestReadOnlyHandlerBlocksMutations(t *testing.T) {
	ts, _, _, srv, _ := newReadOnlyTestServer()
	defer ts.Close()
	defer srv.Stop()

	// POST /register should return 404 (not registered on read-only mux)
	body, _ := json.Marshal(map[string]string{"container_id": "test"})
	resp, err := http.Post(ts.URL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /register: status = %d, want 404", resp.StatusCode)
	}

	// POST /containers should return 405 (GET-only wrapper)
	resp, err = http.Post(ts.URL+"/containers", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /containers: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /containers: status = %d, want 405", resp.StatusCode)
	}

	// DELETE should return 404 (no /containers/ route)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/containers/test123", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /containers/test123: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE /containers/test123: status = %d, want 404", resp.StatusCode)
	}

	// PUT should return 404 (no /containers/ route)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/containers/test123/limits", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /containers/test123/limits: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT /containers/test123/limits: status = %d, want 404", resp.StatusCode)
	}
}

func TestReadOnlyHandlerNoContainerSubroutes(t *testing.T) {
	ts, ms, _, srv, _ := newReadOnlyTestServer()
	defer ts.Close()
	defer srv.Stop()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")

	// GET /containers/{id}/limits should return 404
	resp, err := http.Get(ts.URL + "/containers/" + c.ID + "/limits")
	if err != nil {
		t.Fatalf("GET /containers/{id}/limits: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /containers/{id}/limits: status = %d, want 404", resp.StatusCode)
	}

	// GET /containers/{id}/usage should return 404
	resp, err = http.Get(ts.URL + "/containers/" + c.ID + "/usage")
	if err != nil {
		t.Fatalf("GET /containers/{id}/usage: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /containers/{id}/usage: status = %d, want 404", resp.StatusCode)
	}

	// POST /containers/{id}/clone should return 404
	resp, err = http.Post(ts.URL+"/containers/"+c.ID+"/clone", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /containers/{id}/clone: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /containers/{id}/clone: status = %d, want 404", resp.StatusCode)
	}
}

func wsURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

func TestWebSocketConnection(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/events"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want 101", resp.StatusCode)
	}
}

func TestWebSocketReceivesLimitChange(t *testing.T) {
	ts, ms, _, _, _, bus := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	ms.RegisterContainer(dockerID, "test")
	containerID := dockerID[:12]

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/events"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Allow server handler goroutine to subscribe before publishing
	time.Sleep(50 * time.Millisecond)

	// Publish a limit_change event
	bus.PublishData(events.LimitChange, containerID, events.LimitChangeData{
		LimitType: "cpu",
		OldValue:  100,
		NewValue:  200,
		Operation: "increase",
	})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var evt events.Event
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatalf("read: %v", err)
	}
	if evt.Type != events.LimitChange {
		t.Errorf("type = %s, want limit_change", evt.Type)
	}
	if evt.ContainerID != containerID {
		t.Errorf("container = %s, want %s", evt.ContainerID, containerID)
	}
}

func TestWebSocketFilterByType(t *testing.T) {
	ts, _, _, _, _, bus := newTestServer()
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/events?types=limit_change"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Publish usage_update (should be filtered out) then limit_change
	bus.PublishData(events.UsageUpdate, "c1", events.UsageUpdateData{})
	bus.PublishData(events.LimitChange, "c1", events.LimitChangeData{LimitType: "cpu"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var evt events.Event
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatalf("read: %v", err)
	}
	if evt.Type != events.LimitChange {
		t.Errorf("type = %s, want limit_change (filter should exclude usage_update)", evt.Type)
	}
}

func TestWebSocketFilterByContainerID(t *testing.T) {
	ts, _, _, _, _, bus := newTestServer()
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/events?container_id=c2"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Allow server handler goroutine to subscribe before publishing
	time.Sleep(50 * time.Millisecond)

	bus.PublishData(events.LimitChange, "c1", events.LimitChangeData{LimitType: "cpu"})
	bus.PublishData(events.LimitChange, "c2", events.LimitChangeData{LimitType: "ram"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var evt events.Event
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatalf("read: %v", err)
	}
	if evt.ContainerID != "c2" {
		t.Errorf("container = %s, want c2 (filter should exclude c1)", evt.ContainerID)
	}
}

func TestOllamaQueueEndpointNotConfigured(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ollama/queue")
	if err != nil {
		t.Fatalf("GET /ollama/queue: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (ollama not configured)", resp.StatusCode)
	}
}

func TestOllamaModelsEndpointNotConfigured(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ollama/models")
	if err != nil {
		t.Fatalf("GET /ollama/models: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (ollama not configured)", resp.StatusCode)
	}
}

func TestProvidersEndpoint(t *testing.T) {
	ts, _, _, _, _, _ := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/providers")
	if err != nil {
		t.Fatalf("GET /providers: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["ollama_available"] != false {
		t.Errorf("ollama_available = %v, want false", result["ollama_available"])
	}
}

func TestDeleteContainerRemovesOllama(t *testing.T) {
	// When ollama is nil, delete should not crash
	ts, ms, _, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	c, _ := ms.RegisterContainer(dockerID, "test")

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/containers/"+c.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRegisterIncludesOllamaAvailable(t *testing.T) {
	ts, _, md, _, _, _ := newTestServer()
	defer ts.Close()

	dockerID := "abcdef123456789000"
	md.AddContainer(dockerID, "test-container", true)

	body, _ := json.Marshal(map[string]string{"container_id": dockerID})
	resp, err := http.Post(ts.URL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["ollama_available"] != false {
		t.Errorf("ollama_available = %v, want false", result["ollama_available"])
	}
}

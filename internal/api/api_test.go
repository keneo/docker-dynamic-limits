package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/testutil"
)

func newTestServer() (*httptest.Server, *testutil.MockStore, *testutil.MockDocker, *testutil.MockEnforcement, *testutil.MockProxy) {
	ms := testutil.NewMockStore()
	md := testutil.NewMockDocker()
	me := testutil.NewMockEnforcement()
	mp := testutil.NewMockProxy()
	srv := NewServer(ms, md, me, mp)
	ts := httptest.NewServer(srv.Handler())
	return ts, ms, md, me, mp
}

func newReadOnlyTestServer() (*httptest.Server, *testutil.MockStore, *testutil.MockDocker, *Server) {
	ms := testutil.NewMockStore()
	md := testutil.NewMockDocker()
	me := testutil.NewMockEnforcement()
	mp := testutil.NewMockProxy()
	srv := NewServer(ms, md, me, mp)
	ts := httptest.NewServer(srv.ReadOnlyHandler())
	return ts, ms, md, srv
}

func TestListContainersEmpty(t *testing.T) {
	ts, _, _, _, _ := newTestServer()
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
	ts, _, md, me, _ := newTestServer()
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
	ts, _, _, _, _ := newTestServer()
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
	ts, _, _, _, _ := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, _, _, _, _ := newTestServer()
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
	ts, ms, _, me, _ := newTestServer()
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
	ts, ms, _, me, _ := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, ms, _, _, mp := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, _, _, _, _ := newTestServer()
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
	ts, ms, _, _, _ := newTestServer()
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
	ts, _, _, _, _ := newTestServer()
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
	ts, ms, md, srv := newReadOnlyTestServer()
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
	ts, ms, md, srv := newReadOnlyTestServer()
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
	ts, _, _, srv := newReadOnlyTestServer()
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
	ts, ms, _, srv := newReadOnlyTestServer()
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

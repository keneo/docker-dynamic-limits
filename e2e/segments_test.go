//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// apiPostExpect performs a POST and expects a specific status code.
func apiPostExpect(t *testing.T, path string, body interface{}, statusCode int, out interface{}) {
	t.Helper()
	jsonBody, _ := json.Marshal(body)
	resp, err := httpClient.Post(daemonURL+path, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("POST %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != statusCode {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s returned %d (want %d): %s", path, resp.StatusCode, statusCode, string(respBody))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("POST %s decode error: %v", path, err)
		}
	}
}

// apiPutExpect performs a PUT and expects a specific status code.
func apiPutExpect(t *testing.T, path string, body interface{}, statusCode int, out interface{}) {
	t.Helper()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, daemonURL+path, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != statusCode {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s returned %d (want %d): %s", path, resp.StatusCode, statusCode, string(respBody))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("PUT %s decode error: %v", path, err)
		}
	}
}

// createSegment creates a segment via the API and returns the segment ID.
func createSegment(t *testing.T, id, name string) string {
	t.Helper()
	var result map[string]interface{}
	apiPostExpect(t, "/segments", map[string]string{"id": id, "name": name}, http.StatusCreated, &result)
	t.Cleanup(func() {
		// Best-effort cleanup
		req, _ := http.NewRequest(http.MethodDelete, daemonURL+"/segments/"+id, nil)
		httpClient.Do(req)
	})
	return id
}

// assignContainer assigns a container to a segment.
func assignContainer(t *testing.T, containerID, segmentID string) {
	t.Helper()
	apiPost(t, fmt.Sprintf("/segments/%s/containers/%s/assign", segmentID, containerID), nil, nil)
}

// unassignContainer removes a container from a segment.
func unassignContainer(t *testing.T, containerID, segmentID string) {
	t.Helper()
	apiPost(t, fmt.Sprintf("/segments/%s/containers/%s/unassign", segmentID, containerID), nil, nil)
}

// setSegmentLimit sets a limit on a segment.
func setSegmentLimit(t *testing.T, segmentID, limitType string, value int64) {
	t.Helper()
	apiPutExpect(t, fmt.Sprintf("/segments/%s/limits", segmentID),
		map[string]interface{}{
			"type":      limitType,
			"value":     value,
			"operation": "set",
		}, http.StatusOK, nil)
}

// --- Tests ---

func TestSegmentCRUD(t *testing.T) {
	segID := uniqueName("seg-crud")

	// Create
	var createResult map[string]interface{}
	apiPostExpect(t, "/segments", map[string]string{"id": segID, "name": "Test CRUD"}, http.StatusCreated, &createResult)
	if createResult["id"] != segID {
		t.Errorf("create: id = %v, want %s", createResult["id"], segID)
	}

	// List — should contain our segment
	var listResult struct {
		Segments []map[string]interface{} `json:"segments"`
	}
	apiGet(t, "/segments", &listResult)
	found := false
	for _, s := range listResult.Segments {
		if s["id"] == segID {
			found = true
			if s["name"] != "Test CRUD" {
				t.Errorf("name = %v, want Test CRUD", s["name"])
			}
		}
	}
	if !found {
		t.Fatalf("segment %s not found in list", segID)
	}

	// Detail
	var detail map[string]interface{}
	apiGet(t, "/segments/"+segID, &detail)
	seg := detail["segment"].(map[string]interface{})
	if seg["id"] != segID {
		t.Errorf("detail: id = %v, want %s", seg["id"], segID)
	}

	// Delete
	apiDelete(t, "/segments/"+segID, nil)

	// List again — should not contain deleted segment
	apiGet(t, "/segments", &listResult)
	for _, s := range listResult.Segments {
		if s["id"] == segID {
			t.Errorf("segment %s still in list after delete", segID)
		}
	}
}

func TestSegmentAssignAndList(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-assign"), "Assign Test")

	// Create and register 2 containers
	name1 := uniqueName("seg-c1")
	name2 := uniqueName("seg-c2")
	dockerID1 := createIdleContainer(t, name1)
	dockerID2 := createIdleContainer(t, name2)
	cid1 := registerContainer(t, dockerID1)
	cid2 := registerContainer(t, dockerID2)

	// Assign both to segment
	assignContainer(t, cid1, segID)
	assignContainer(t, cid2, segID)

	// List segment containers
	var listResult struct {
		Containers []map[string]interface{} `json:"containers"`
	}
	apiGet(t, "/segments/"+segID+"/containers", &listResult)

	if len(listResult.Containers) != 2 {
		t.Fatalf("segment containers count = %d, want 2", len(listResult.Containers))
	}

	// Unassign one
	unassignContainer(t, cid1, segID)

	apiGet(t, "/segments/"+segID+"/containers", &listResult)
	if len(listResult.Containers) != 1 {
		t.Fatalf("after unassign: segment containers count = %d, want 1", len(listResult.Containers))
	}

	// Verify the remaining container is cid2
	c := listResult.Containers[0]["container"].(map[string]interface{})
	if c["id"] != cid2 {
		t.Errorf("remaining container = %v, want %s", c["id"], cid2)
	}
}

func TestSegmentLimitsAndUsage(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-lim"), "Limits Test")

	// Set segment limits
	setSegmentLimit(t, segID, "cpu", 86400)
	setSegmentLimit(t, segID, "spending", 1000000)

	// Get segment limits
	var limits map[string]interface{}
	apiGet(t, "/segments/"+segID+"/limits", &limits)

	if limits["cpu"].(float64) != 86400 {
		t.Errorf("cpu limit = %v, want 86400", limits["cpu"])
	}
	if limits["spending"].(float64) != 1000000 {
		t.Errorf("spending limit = %v, want 1000000", limits["spending"])
	}

	// Create and assign a container
	name := uniqueName("seg-lim-c")
	dockerID := createIdleContainer(t, name)
	cid := registerContainer(t, dockerID)
	assignContainer(t, cid, segID)

	// Verify segment usage endpoint returns valid response with limits
	var usageResp struct {
		Usage   map[string]float64 `json:"usage"`
		Limits  map[string]float64 `json:"limits"`
	}
	apiGet(t, "/segments/"+segID+"/usage", &usageResp)

	if usageResp.Limits["cpu"] != 86400 {
		t.Errorf("segment cpu limit = %v, want 86400", usageResp.Limits["cpu"])
	}
	if usageResp.Limits["spending"] != 1000000 {
		t.Errorf("segment spending limit = %v, want 1000000", usageResp.Limits["spending"])
	}
}

func TestSegmentDetail(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-det"), "Detail Test")

	// Set a limit
	setSegmentLimit(t, segID, "spending", 500000)

	// Register and assign container
	name := uniqueName("seg-det-c")
	dockerID := createIdleContainer(t, name)
	cid := registerContainer(t, dockerID)
	assignContainer(t, cid, segID)

	// Get detail
	var detail map[string]interface{}
	apiGet(t, "/segments/"+segID, &detail)

	seg := detail["segment"].(map[string]interface{})
	if seg["id"] != segID {
		t.Errorf("segment id = %v, want %s", seg["id"], segID)
	}

	containers := detail["containers"].(float64)
	if containers != 1 {
		t.Errorf("containers = %v, want 1", containers)
	}

	limits := detail["limits"].(map[string]interface{})
	if limits["spending"].(float64) != 500000 {
		t.Errorf("spending limit = %v, want 500000", limits["spending"])
	}
}

func TestSegmentContainerPathDelegation(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-path"), "Path Test")

	// Register and assign container
	name := uniqueName("seg-path-c")
	dockerID := createIdleContainer(t, name)
	cid := registerContainer(t, dockerID)
	assignContainer(t, cid, segID)

	// Set container limit via segment-scoped path
	apiPutExpect(t, fmt.Sprintf("/segments/%s/containers/%s/limits", segID, cid),
		map[string]interface{}{
			"type":      "cpu",
			"value":     3600,
			"operation": "set",
		}, http.StatusOK, nil)

	// Verify limit was set via direct container path
	var limits map[string]interface{}
	apiGet(t, fmt.Sprintf("/containers/%s/limits", cid), &limits)

	if limits["cpu"].(float64) != 3600 {
		t.Errorf("cpu limit = %v, want 3600", limits["cpu"])
	}

	// Get container usage via segment path
	var usage map[string]interface{}
	apiGet(t, fmt.Sprintf("/segments/%s/containers/%s/usage", segID, cid), &usage)
	// Just verify it doesn't error — response should be valid JSON
	if usage == nil {
		t.Error("usage response was nil")
	}
}

func TestSegmentContainerNotMember(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-nomem"), "No Member Test")

	// Register a container but DON'T assign it to the segment
	name := uniqueName("seg-nomem-c")
	dockerID := createIdleContainer(t, name)
	cid := registerContainer(t, dockerID)

	// Try to access it via segment path — should get 403
	jsonBody, _ := json.Marshal(map[string]interface{}{
		"type":      "cpu",
		"value":     3600,
		"operation": "set",
	})
	req, _ := http.NewRequest(http.MethodPut,
		daemonURL+fmt.Sprintf("/segments/%s/containers/%s/limits", segID, cid),
		bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d (want 403), body: %s", resp.StatusCode, string(body))
	}
}

func TestSegmentDeleteWithContainers(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-delfail"), "Delete Fail Test")

	// Register and assign container
	name := uniqueName("seg-delfail-c")
	dockerID := createIdleContainer(t, name)
	cid := registerContainer(t, dockerID)
	assignContainer(t, cid, segID)

	// Try to delete — should fail
	req, _ := http.NewRequest(http.MethodDelete, daemonURL+"/segments/"+segID, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("delete with containers: status = %d (want 409), body: %s", resp.StatusCode, string(body))
	}

	// Unassign and retry
	unassignContainer(t, cid, segID)
	apiDelete(t, "/segments/"+segID, nil)
}

func TestRegisterWithSegment(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-reg"), "Register Test")

	name := uniqueName("seg-reg-c")
	dockerID := createIdleContainer(t, name)

	// Register with segment_id
	var result struct {
		ID string `json:"id"`
	}
	apiPostExpect(t, "/register", map[string]string{
		"container_id": dockerID,
		"segment_id":   segID,
	}, http.StatusOK, &result)

	if result.ID == "" {
		t.Fatal("register returned empty ID")
	}

	// Verify container is in segment
	var listResult struct {
		Containers []map[string]interface{} `json:"containers"`
	}
	apiGet(t, "/segments/"+segID+"/containers", &listResult)

	if len(listResult.Containers) != 1 {
		t.Fatalf("segment containers = %d, want 1", len(listResult.Containers))
	}

	c := listResult.Containers[0]["container"].(map[string]interface{})
	if c["id"] != result.ID {
		t.Errorf("container id = %v, want %s", c["id"], result.ID)
	}
}

func TestSegmentFreezeUnfreeze(t *testing.T) {
	segID := createSegment(t, uniqueName("seg-frz"), "Freeze Test")

	// Create and assign 2 containers
	name1 := uniqueName("seg-frz-c1")
	name2 := uniqueName("seg-frz-c2")
	dockerID1 := createIdleContainer(t, name1)
	dockerID2 := createIdleContainer(t, name2)
	cid1 := registerContainer(t, dockerID1)
	cid2 := registerContainer(t, dockerID2)
	assignContainer(t, cid1, segID)
	assignContainer(t, cid2, segID)

	// Freeze segment
	var freezeResult map[string]interface{}
	apiPost(t, "/segments/"+segID+"/freeze-all", nil, &freezeResult)

	count := freezeResult["count"].(float64)
	if count != 2 {
		t.Errorf("freeze count = %v, want 2", count)
	}

	// Verify containers are paused
	ok := pollUntil(5*time.Second, 500*time.Millisecond, func() bool {
		info1, _ := dockerCli.ContainerInspect(context.Background(), dockerID1)
		info2, _ := dockerCli.ContainerInspect(context.Background(), dockerID2)
		return info1.State.Paused && info2.State.Paused
	})
	if !ok {
		t.Error("containers not paused within 5s")
	}

	// Unfreeze segment
	var unfreezeResult map[string]interface{}
	apiPost(t, "/segments/"+segID+"/unfreeze-all", nil, &unfreezeResult)

	count = unfreezeResult["count"].(float64)
	if count != 2 {
		t.Errorf("unfreeze count = %v, want 2", count)
	}

	// Verify containers are running
	ok = pollUntil(5*time.Second, 500*time.Millisecond, func() bool {
		info1, _ := dockerCli.ContainerInspect(context.Background(), dockerID1)
		info2, _ := dockerCli.ContainerInspect(context.Background(), dockerID2)
		return info1.State.Running && !info1.State.Paused && info2.State.Running && !info2.State.Paused
	})
	if !ok {
		t.Error("containers not running within 5s after unfreeze")
	}
}

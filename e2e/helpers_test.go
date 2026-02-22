//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

var nameCounter uint64

func uniqueName(prefix string) string {
	n := atomic.AddUint64(&nameCounter, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, n, time.Now().UnixNano())
}

// createBusyContainer creates and starts an alpine container running "yes > /dev/null".
func createBusyContainer(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()

	resp, err := dockerCli.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"sh", "-c", "yes > /dev/null"},
	}, nil, nil, nil, name)
	if err != nil {
		t.Fatalf("failed to create busy container: %v", err)
	}

	if err := dockerCli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		t.Fatalf("failed to start busy container: %v", err)
	}

	t.Cleanup(func() {
		dockerCli.ContainerRemove(context.Background(), resp.ID, types.ContainerRemoveOptions{Force: true})
	})

	return resp.ID
}

// createIdleContainer creates and starts an alpine container running "sleep infinity".
func createIdleContainer(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()

	resp, err := dockerCli.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"sleep", "infinity"},
	}, nil, nil, nil, name)
	if err != nil {
		t.Fatalf("failed to create idle container: %v", err)
	}

	if err := dockerCli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		t.Fatalf("failed to start idle container: %v", err)
	}

	t.Cleanup(func() {
		dockerCli.ContainerRemove(context.Background(), resp.ID, types.ContainerRemoveOptions{Force: true})
	})

	return resp.ID
}

// apiGet performs a GET request and decodes the JSON response.
func apiGet(t *testing.T, path string, out interface{}) {
	t.Helper()
	resp, err := httpClient.Get(daemonURL + path)
	if err != nil {
		t.Fatalf("GET %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("GET %s decode error: %v", path, err)
		}
	}
}

// apiGetWithHeader performs a GET request with a custom header.
func apiGetWithHeader(t *testing.T, path string, headerKey, headerVal string, out interface{}) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, daemonURL+path, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(headerKey, headerVal)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("GET %s decode error: %v", path, err)
		}
	}
}

// apiPost performs a POST request with JSON body and decodes the response.
func apiPost(t *testing.T, path string, body interface{}, out interface{}) {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	resp, err := httpClient.Post(daemonURL+path, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("POST %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("POST %s decode error: %v", path, err)
		}
	}
}

// apiPut performs a PUT request with JSON body and decodes the response.
func apiPut(t *testing.T, path string, body interface{}, out interface{}) {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, daemonURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("PUT %s decode error: %v", path, err)
		}
	}
}

// apiDelete performs a DELETE request and decodes the response.
func apiDelete(t *testing.T, path string, out interface{}) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, daemonURL+path, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("DELETE %s decode error: %v", path, err)
		}
	}
}

// registerContainer registers a Docker container with ddld and returns the short container ID.
func registerContainer(t *testing.T, dockerID string) string {
	t.Helper()
	var result struct {
		ID       string `json:"id"`
		DockerID string `json:"docker_id"`
		Name     string `json:"name"`
	}
	apiPost(t, "/register", map[string]string{"container_id": dockerID}, &result)
	if result.ID == "" {
		t.Fatal("register returned empty ID")
	}
	return result.ID
}

// setLimit sets a limit for a container via the API.
func setLimit(t *testing.T, containerID string, limitType string, value int64) {
	t.Helper()
	apiPut(t, fmt.Sprintf("/containers/%s/limits", containerID), map[string]interface{}{
		"type":  limitType,
		"value": value,
	}, nil)
}

// pollUntil polls a check function until it returns true or timeout.
func pollUntil(timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// waitForContainerPaused waits until the Docker container is paused.
func waitForContainerPaused(t *testing.T, dockerID string, timeout time.Duration) {
	t.Helper()
	ok := pollUntil(timeout, 500*time.Millisecond, func() bool {
		info, err := dockerCli.ContainerInspect(context.Background(), dockerID)
		if err != nil {
			return false
		}
		return info.State.Paused
	})
	if !ok {
		t.Fatalf("container %s did not become paused within %s", dockerID[:12], timeout)
	}
}

// waitForContainerRunning waits until the Docker container is running (not paused).
func waitForContainerRunning(t *testing.T, dockerID string, timeout time.Duration) {
	t.Helper()
	ok := pollUntil(timeout, 500*time.Millisecond, func() bool {
		info, err := dockerCli.ContainerInspect(context.Background(), dockerID)
		if err != nil {
			return false
		}
		return info.State.Running && !info.State.Paused
	})
	if !ok {
		t.Fatalf("container %s did not become running within %s", dockerID[:12], timeout)
	}
}

// getContainerStatus fetches the full status of a registered container from ddld.
func getContainerStatus(t *testing.T, containerID string) map[string]interface{} {
	t.Helper()
	var status map[string]interface{}
	apiGet(t, fmt.Sprintf("/containers/%s", containerID), &status)
	return status
}

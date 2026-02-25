//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

func TestSelfQueryUsage(t *testing.T) {
	name := uniqueName("e2e-squsage")
	dockerID := createBusyContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Set a high CPU limit so enforcement doesn't pause us
	setLimit(t, containerID, "cpu", 999999)

	// Wait for some CPU usage to accumulate and be recorded
	time.Sleep(3 * time.Second)

	// Query usage via self-query endpoint
	var result struct {
		Usage  map[string]interface{} `json:"usage"`
		Limits map[string]interface{} `json:"limits"`
	}
	apiGet(t, fmt.Sprintf("/usage?id=%s", containerID), &result)

	cpuUsage, ok := result.Usage["cpu"].(float64)
	if !ok {
		t.Fatalf("expected cpu in usage, got %v", result.Usage)
	}
	if cpuUsage <= 0 {
		t.Fatalf("expected cpu usage > 0, got %v", cpuUsage)
	}
}

func TestSelfQueryLimits(t *testing.T) {
	name := uniqueName("e2e-sqlimits")
	dockerID := createIdleContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Set some limits
	setLimit(t, containerID, "cpu", 7200)
	setLimit(t, containerID, "ram", 256*1024*1024)

	// Query limits via self-query endpoint
	var limits map[string]interface{}
	apiGet(t, fmt.Sprintf("/limits?id=%s", containerID), &limits)

	if cpuLimit, ok := limits["cpu"].(float64); !ok || int64(cpuLimit) != 7200 {
		t.Fatalf("expected cpu limit=7200, got %v", limits["cpu"])
	}
	expectedRAM := int64(256 * 1024 * 1024)
	if ramLimit, ok := limits["ram"].(float64); !ok || int64(ramLimit) != expectedRAM {
		t.Fatalf("expected ram limit=%d, got %v", expectedRAM, limits["ram"])
	}
}


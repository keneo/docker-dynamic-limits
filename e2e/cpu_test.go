//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestCPULimitPausesContainer(t *testing.T) {
	name := uniqueName("e2e-cpu")
	dockerID := createBusyContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Let the container accumulate some CPU usage
	time.Sleep(2 * time.Second)

	// Set a very low CPU limit (1 second) — the container should already exceed it
	setLimit(t, containerID, "cpu", 1)

	// Wait for the container to be paused by enforcement
	waitForContainerPaused(t, dockerID, 10*time.Second)

	// Verify enforcement state via API
	status := getContainerStatus(t, containerID)
	enforced, ok := status["enforced"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'enforced' in status")
	}
	if enforced["cpu"] != true {
		t.Fatalf("expected enforced.cpu=true, got %v", enforced["cpu"])
	}
}

func TestCPULimitIncreaseUnpauses(t *testing.T) {
	name := uniqueName("e2e-cpuup")
	dockerID := createBusyContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Let CPU accumulate
	time.Sleep(2 * time.Second)

	// Set low limit to trigger pause
	setLimit(t, containerID, "cpu", 1)
	waitForContainerPaused(t, dockerID, 10*time.Second)

	// Increase limit well beyond current usage to trigger unpause
	setLimit(t, containerID, "cpu", 999999)

	// Wait for container to be unpaused
	waitForContainerRunning(t, dockerID, 10*time.Second)

	// Verify enforcement state
	status := getContainerStatus(t, containerID)
	enforced, ok := status["enforced"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'enforced' in status")
	}
	if enforced["cpu"] == true {
		t.Fatal("expected enforced.cpu to be false after limit increase")
	}
}

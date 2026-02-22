//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"

	"github.com/docker/docker/api/types"
)

func TestCloneContainerWithLimits(t *testing.T) {
	name := uniqueName("e2e-clone")
	dockerID := createIdleContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Set limits on the source container
	setLimit(t, containerID, "cpu", 3600)
	setLimit(t, containerID, "ram", 128*1024*1024)

	// Clone the container
	var cloned struct {
		ID       string `json:"id"`
		DockerID string `json:"docker_id"`
		Name     string `json:"name"`
	}
	cloneName := uniqueName("e2e-cloned")
	apiPost(t, fmt.Sprintf("/containers/%s/clone", containerID), map[string]string{
		"name": cloneName,
	}, &cloned)

	if cloned.ID == "" {
		t.Fatal("clone returned empty ID")
	}

	// Cleanup the cloned container
	t.Cleanup(func() {
		dockerCli.ContainerRemove(context.Background(), cloned.DockerID, types.ContainerRemoveOptions{Force: true})
	})

	// Verify the clone has the same limits
	var limits map[string]interface{}
	apiGet(t, fmt.Sprintf("/containers/%s/limits", cloned.ID), &limits)

	// JSON numbers decode as float64
	if cpuLimit, ok := limits["cpu"].(float64); !ok || int64(cpuLimit) != 3600 {
		t.Fatalf("expected cloned cpu limit=3600, got %v", limits["cpu"])
	}
	expectedRAM := int64(128 * 1024 * 1024)
	if ramLimit, ok := limits["ram"].(float64); !ok || int64(ramLimit) != expectedRAM {
		t.Fatalf("expected cloned ram limit=%d, got %v", expectedRAM, limits["ram"])
	}
}

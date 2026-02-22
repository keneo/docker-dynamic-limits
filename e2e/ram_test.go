//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

func TestRAMLimitApplied(t *testing.T) {
	name := uniqueName("e2e-ram")
	dockerID := createIdleContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Set RAM limit to 64 MiB
	ramLimit := int64(64 * 1024 * 1024)
	setLimit(t, containerID, "ram", ramLimit)

	// Wait for the limit to be applied via Docker
	time.Sleep(2 * time.Second)

	// Verify via Docker inspect that the memory limit is set
	info, err := dockerCli.ContainerInspect(context.Background(), dockerID)
	if err != nil {
		t.Fatalf("failed to inspect container: %v", err)
	}

	if info.HostConfig.Memory != ramLimit {
		t.Fatalf("expected Memory=%d, got %d", ramLimit, info.HostConfig.Memory)
	}
}

//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

func TestRegisterAndList(t *testing.T) {
	name := uniqueName("e2e-reg")
	dockerID := createIdleContainer(t, name)

	// Register the container
	containerID := registerContainer(t, dockerID)

	// Verify it appears in the listing
	var listing []map[string]interface{}
	apiGet(t, "/containers", &listing)

	found := false
	for _, entry := range listing {
		c, ok := entry["container"].(map[string]interface{})
		if !ok {
			continue
		}
		if c["id"] == containerID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("container %s not found in listing", containerID)
	}

	// Verify individual GET returns it
	var status map[string]interface{}
	apiGet(t, fmt.Sprintf("/containers/%s", containerID), &status)
	c := status["container"].(map[string]interface{})
	if c["id"] != containerID {
		t.Fatalf("expected container ID %s, got %v", containerID, c["id"])
	}
}

func TestRemoveContainer(t *testing.T) {
	name := uniqueName("e2e-rm")
	dockerID := createIdleContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Delete it
	var result map[string]string
	apiDelete(t, fmt.Sprintf("/containers/%s", containerID), &result)
	if result["status"] != "removed" {
		t.Fatalf("expected status 'removed', got %q", result["status"])
	}

	// Verify it's gone from listing
	var listing []map[string]interface{}
	apiGet(t, "/containers", &listing)

	for _, entry := range listing {
		c, ok := entry["container"].(map[string]interface{})
		if !ok {
			continue
		}
		if c["id"] == containerID {
			t.Fatalf("container %s should have been removed but still in listing", containerID)
		}
	}
}

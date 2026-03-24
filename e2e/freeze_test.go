//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

func TestFreezeSkipsEnforcement(t *testing.T) {
	name := uniqueName("e2e-freeze")
	dockerID := createBusyContainer(t, name)

	containerID := registerContainer(t, dockerID)

	// Let it accumulate CPU
	time.Sleep(2 * time.Second)

	// Set a very low CPU limit — container would be paused if enforcement runs
	setLimit(t, containerID, "cpu", 1)

	// Wait for enforcement to pause it
	waitForContainerPaused(t, dockerID, 10*time.Second)

	// Increase limit to unpause, then freeze before enforcement triggers again
	setLimit(t, containerID, "cpu", 999999)
	waitForContainerRunning(t, dockerID, 10*time.Second)

	// Freeze the container
	apiPost(t, "/containers/"+containerID+"/freeze", nil, nil)

	// Verify it's paused (frozen)
	info, err := dockerCli.ContainerInspect(context.Background(), dockerID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !info.State.Paused {
		t.Fatal("container should be paused after freeze")
	}

	// Now set an extremely low CPU limit that would trigger enforcement
	setLimit(t, containerID, "cpu", 1)

	// Wait a few seconds — enforcement loop ticks every ~1s
	time.Sleep(3 * time.Second)

	// Container should still be paused (frozen), NOT killed/stopped.
	// If enforcement ran, it would have tried to pause an already-paused container
	// (which is fine), but more importantly the frozen optimization means
	// the enforcement loop doesn't even check.
	status := getContainerStatus(t, containerID)

	frozen, _ := status["frozen"].(bool)
	if !frozen {
		t.Error("container should still be frozen")
	}

	// Verify container is still paused in Docker (not stopped/killed)
	info, err = dockerCli.ContainerInspect(context.Background(), dockerID)
	if err != nil {
		t.Fatalf("inspect after wait: %v", err)
	}
	if !info.State.Paused {
		t.Error("container should still be Docker-paused")
	}
	if !info.State.Running {
		t.Error("container should still be running (not killed)")
	}

	// Unfreeze
	apiPost(t, "/containers/"+containerID+"/unfreeze", nil, nil)

	// After unfreeze, enforcement should kick in since CPU limit is 1
	waitForContainerPaused(t, dockerID, 10*time.Second)

	status = getContainerStatus(t, containerID)
	enforced, ok := status["enforced"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'enforced' in status")
	}
	if enforced["cpu"] != true {
		t.Fatal("expected enforced.cpu=true after unfreeze with low limit")
	}
}

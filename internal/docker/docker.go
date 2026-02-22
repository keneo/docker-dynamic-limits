package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Client wraps the Docker Engine API.
type Client struct {
	cli *client.Client
}

// NewClient creates a Docker API client.
func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Close closes the Docker client.
func (c *Client) Close() error {
	return c.cli.Close()
}

// InspectContainer returns full container information.
func (c *Client) InspectContainer(ctx context.Context, id string) (types.ContainerJSON, error) {
	return c.cli.ContainerInspect(ctx, id)
}

// PauseContainer freezes a container using the cgroup freezer.
func (c *Client) PauseContainer(ctx context.Context, id string) error {
	return c.cli.ContainerPause(ctx, id)
}

// UnpauseContainer resumes a frozen container.
func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	return c.cli.ContainerUnpause(ctx, id)
}

// IsContainerPaused returns true if the container is paused.
func (c *Client) IsContainerPaused(ctx context.Context, id string) (bool, error) {
	info, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return false, err
	}
	return info.State.Paused, nil
}

// IsContainerRunning returns true if the container is running (including paused).
func (c *Client) IsContainerRunning(ctx context.Context, id string) (bool, error) {
	info, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return false, err
	}
	return info.State.Running, nil
}

// UpdateMemoryLimit updates the memory limit of a running container.
func (c *Client) UpdateMemoryLimit(ctx context.Context, id string, memoryBytes int64) error {
	_, err := c.cli.ContainerUpdate(ctx, id, container.UpdateConfig{
		Resources: container.Resources{
			Memory: memoryBytes,
		},
	})
	return err
}

// ContainerVethName tries to find the veth interface for a container.
// It reads the container's network settings to find the sandbox key,
// then matches it to a veth on the host.
func (c *Client) ContainerVethName(ctx context.Context, id string) (string, error) {
	info, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	// The sandbox key is the path to the network namespace
	sandboxKey := info.NetworkSettings.SandboxKey
	if sandboxKey == "" {
		return "", fmt.Errorf("no sandbox key for container %s", id[:12])
	}
	// Extract the PID-based interface index from the container's eth0
	// We need to exec into the container to read the iflink
	execCfg := types.ExecConfig{
		Cmd:          []string{"cat", "/sys/class/net/eth0/iflink"},
		AttachStdout: true,
	}
	execResp, err := c.cli.ContainerExecCreate(ctx, id, execCfg)
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	attachResp, err := c.cli.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return "", fmt.Errorf("exec attach: %w", err)
	}
	defer attachResp.Close()

	output, err := io.ReadAll(attachResp.Reader)
	if err != nil {
		return "", fmt.Errorf("read exec output: %w", err)
	}
	// Parse the interface index (strip docker mux header bytes)
	iflink := strings.TrimSpace(string(output))
	// Remove any non-digit prefix (docker stream header)
	for i, ch := range iflink {
		if ch >= '0' && ch <= '9' {
			iflink = iflink[i:]
			break
		}
	}
	iflink = strings.TrimSpace(strings.Split(iflink, "\n")[0])

	// Now find the veth with this ifindex on the host
	// We'll return "veth" prefix + need to look it up from /sys/class/net/
	return fmt.Sprintf("ifindex:%s", iflink), nil
}

// CloneContainer commits the container and creates a new one with the same config.
func (c *Client) CloneContainer(ctx context.Context, sourceID string, newName string) (string, error) {
	// Inspect the source container
	info, err := c.cli.ContainerInspect(ctx, sourceID)
	if err != nil {
		return "", fmt.Errorf("inspect source: %w", err)
	}

	// Commit the container to create a new image
	commitResp, err := c.cli.ContainerCommit(ctx, sourceID, types.ContainerCommitOptions{
		Reference: "ddl-clone-" + sourceID[:12],
		Comment:   "cloned by ddl",
	})
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	// Prepare the new container config based on the original
	config := info.Config
	hostConfig := info.HostConfig

	if newName == "" {
		newName = info.Name + "-clone"
	}
	newName = strings.TrimPrefix(newName, "/")

	// Override the image to the committed snapshot
	config.Image = commitResp.ID

	// Create the new container
	createResp, err := c.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, newName)
	if err != nil {
		return "", fmt.Errorf("create clone: %w", err)
	}

	// Start the new container
	if err := c.cli.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start clone: %w", err)
	}

	return createResp.ID, nil
}

// GetContainerDiskUsage returns the SizeRw (writable layer size) in bytes.
func (c *Client) GetContainerDiskUsage(ctx context.Context, id string) (int64, error) {
	info, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return 0, err
	}
	if info.SizeRw != nil {
		return *info.SizeRw, nil
	}
	return 0, nil
}

// DisconnectNetwork disconnects a container from all networks.
func (c *Client) DisconnectNetwork(ctx context.Context, id string) error {
	info, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	for netName := range info.NetworkSettings.Networks {
		if err := c.cli.NetworkDisconnect(ctx, netName, id, true); err != nil {
			return fmt.Errorf("disconnect from %s: %w", netName, err)
		}
	}
	return nil
}

// ReconnectNetwork reconnects a container to the bridge network.
func (c *Client) ReconnectNetwork(ctx context.Context, id string) error {
	return c.cli.NetworkConnect(ctx, "bridge", id, nil)
}

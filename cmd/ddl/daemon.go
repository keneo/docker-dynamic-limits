package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	daemonContainerName = "ddl-daemon"
	daemonImageName     = "ddld:latest"
)

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the ddld daemon container",
	}

	cmd.AddCommand(daemonStartCmd())
	cmd.AddCommand(daemonStopCmd())
	cmd.AddCommand(daemonStatusCmd())

	return cmd
}

func daemonStartCmd() *cobra.Command {
	var forceBuild bool
	var port int

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the ddld daemon container",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check if container already exists and is running
			state, _ := inspectContainerState(daemonContainerName)
			if state == "running" {
				fmt.Println("ddl-daemon is already running")
				return nil
			}

			// Remove stopped container if it exists
			if state != "" {
				dockerExec("rm", "-f", daemonContainerName)
			}

			// Build image if needed or forced
			if forceBuild || !imageExists(daemonImageName) {
				fmt.Println("Building ddld image...")
				buildCmd := exec.Command("docker", "build", "-t", daemonImageName, ".")
				buildCmd.Stdout = os.Stdout
				buildCmd.Stderr = os.Stderr
				if err := buildCmd.Run(); err != nil {
					return fmt.Errorf("docker build failed: %w", err)
				}
			}

			// Ensure host socket directory exists
			os.MkdirAll("/var/run/ddl", 0755)

			// Run container
			fmt.Println("Starting ddl-daemon...")
			out, err := dockerOutput("run", "-d",
				"--name", daemonContainerName,
				"--pid=host",
				"-v", "ddl-data:/data",
				"-v", "/sys/fs/cgroup:/sys/fs/cgroup:ro",
				"-v", "/var/run/docker.sock:/var/run/docker.sock",
				"-v", "/var/run/ddl:/run/ddl",
				"-p", fmt.Sprintf("%d:7123", port),
				daemonImageName,
				"-db", "/data/ddl.db",
				"-sock", "/run/ddl/ddl.sock",
			)
			if err != nil {
				return fmt.Errorf("docker run failed: %w", err)
			}
			containerID := strings.TrimSpace(out)
			if len(containerID) > 12 {
				containerID = containerID[:12]
			}

			// Wait for readiness (up to 3 seconds)
			// Check both TCP and socket file
			url := fmt.Sprintf("http://localhost:%d/containers", port)
			ready := false
			for i := 0; i < 6; i++ {
				time.Sleep(500 * time.Millisecond)
				resp, err := http.Get(url)
				if err == nil {
					resp.Body.Close()
					// Also check that the socket file appeared
					if _, serr := os.Stat("/var/run/ddl/ddl.sock"); serr == nil {
						ready = true
						break
					}
					ready = true // TCP is up, socket may take a moment
					break
				}
			}

			if ready {
				fmt.Printf("ddl-daemon started (container %s)\n", containerID)
			} else {
				fmt.Printf("ddl-daemon started (container %s) but not yet responding on port %d\n", containerID, port)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&forceBuild, "build", false, "Force rebuild of the ddld image")
	cmd.Flags().IntVar(&port, "port", 7123, "Host port to expose the API on")

	return cmd
}

func daemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the ddld daemon container",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, _ := inspectContainerState(daemonContainerName)
			if state == "" {
				fmt.Println("ddl-daemon is not running")
				return nil
			}

			if state == "running" {
				if _, err := dockerOutput("stop", daemonContainerName); err != nil {
					return fmt.Errorf("docker stop failed: %w", err)
				}
			}

			if _, err := dockerOutput("rm", daemonContainerName); err != nil {
				return fmt.Errorf("docker rm failed: %w", err)
			}

			fmt.Println("ddl-daemon stopped")
			return nil
		},
	}
}

func daemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show ddld daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := dockerOutput("inspect",
				"--format", "{{.State.Status}} {{.State.StartedAt}} {{.Id}}",
				daemonContainerName,
			)
			if err != nil {
				fmt.Println("ddl-daemon is not running")
				return nil
			}

			parts := strings.Fields(strings.TrimSpace(out))
			if len(parts) < 3 {
				fmt.Println("ddl-daemon is not running")
				return nil
			}

			status := parts[0]
			startedAt := parts[1]
			containerID := parts[2]
			if len(containerID) > 12 {
				containerID = containerID[:12]
			}

			if status == "running" {
				t, err := time.Parse(time.RFC3339Nano, startedAt)
				uptime := "unknown"
				if err == nil {
					uptime = time.Since(t).Round(time.Second).String()
				}
				fmt.Printf("ddl-daemon is running (container %s, uptime %s)\n", containerID, uptime)
			} else {
				fmt.Printf("ddl-daemon is %s (container %s)\n", status, containerID)
			}
			return nil
		},
	}
}

// inspectContainerState returns the state of a container ("running", "exited", etc.)
// or empty string if the container does not exist.
func inspectContainerState(name string) (string, error) {
	out, err := dockerOutput("inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func imageExists(name string) bool {
	_, err := dockerOutput("image", "inspect", name)
	return err == nil
}

func dockerExec(args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dockerOutput(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

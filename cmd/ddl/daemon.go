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

func logsCmd() *cobra.Command {
	var follow bool
	var tail string

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show ddld daemon logs",
		Long: `Show logs from the ddl-daemon container.

Examples:
  ddl logs              Show all logs
  ddl logs -f           Follow log output
  ddl logs -n 50        Show last 50 lines
  ddl logs -f -n 100    Follow, starting from last 100 lines`,
		RunE: func(cmd *cobra.Command, args []string) error {
			state, _ := inspectContainerState(daemonContainerName)
			if state == "" {
				return fmt.Errorf("ddl-daemon is not running")
			}

			dockerArgs := []string{"logs"}
			if follow {
				dockerArgs = append(dockerArgs, "-f")
			}
			if tail != "" {
				dockerArgs = append(dockerArgs, "--tail", tail)
			}
			dockerArgs = append(dockerArgs, daemonContainerName)

			return dockerExec(dockerArgs...)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().StringVarP(&tail, "tail", "n", "", "Number of lines to show from the end")

	return cmd
}

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the ddld daemon container",
		Long: `Manage the ddld daemon container lifecycle.

The daemon runs as a Docker container and provides the API for
managing resource limits. It requires Docker socket access and
cgroup filesystem for enforcement.

Subcommands:
  start    Build image (if needed) and start the daemon container
  stop     Stop and remove the daemon container
  restart  Rebuild image while running, then swap (minimal downtime)
  status   Show whether the daemon is running and its uptime`,
	}

	cmd.AddCommand(daemonStartCmd())
	cmd.AddCommand(daemonStopCmd())
	cmd.AddCommand(daemonRestartCmd())
	cmd.AddCommand(daemonStatusCmd())

	return cmd
}

func daemonStartCmd() *cobra.Command {
	var forceBuild bool
	var port int

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the ddld daemon container",
		Long: `Start the ddld daemon as a Docker container.

Builds the Docker image on first run (or with --build). The daemon
container mounts the Docker socket, cgroup filesystem, and a
persistent volume for the SQLite database.

Environment variables:
  DDL_ANTHROPIC_API_KEY     Anthropic API key for spending proxy
  DDL_OPENAI_API_KEY        OpenAI API key for spending proxy
  DDL_OLLAMA_URL            Ollama server URL (e.g. http://192.168.1.100:11434)
  DDL_OLLAMA_MODELS         Comma-separated allowed model names
  DDL_OLLAMA_MAX_QUEUE      Max queue size (default 50)
  DDL_OLLAMA_TIMEOUT        Request timeout (default 120s)
  DDL_OLLAMA_DEFAULT_BID    Default bid in milli-cents/wall-second (default 0)
  DDL_ENABLE_OPENAI         Enable OpenAI proxy (default: true if key set)
  DDL_ENABLE_ANTHROPIC      Enable Anthropic proxy (default: true if key set)
  DDL_ENABLE_OLLAMA         Enable Ollama proxy (default: true if URL set)

Examples:
  ddl daemon start
  ddl daemon start --build
  ddl daemon start --port 8080
  DDL_ANTHROPIC_API_KEY=sk-ant-... ddl daemon start --build
  DDL_OLLAMA_URL=http://gpu-server:11434 DDL_OLLAMA_MODELS=llama3.2:3b ddl daemon start --build`,
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
				if err := buildImage(); err != nil {
					return err
				}
			}

			return startDaemonContainer(port)
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
		Long:  `Stop and remove the ddl-daemon container. Data is preserved on the ddl-data Docker volume.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return stopDaemon()
		},
	}
}

func stopDaemon() error {
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
}

func buildImage() error {
	fmt.Println("Building ddld image...")
	buildCmd := exec.Command("docker", "build", "-t", daemonImageName, ".")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}
	return nil
}

func startDaemonContainer(port int) error {
	// Ensure host socket directory exists
	os.MkdirAll("/var/run/ddl", 0755)

	// Run container
	fmt.Println("Starting ddl-daemon...")
	runArgs := []string{"run", "-d",
		"--name", daemonContainerName,
		"--pid=host",
		"-v", "ddl-data:/data",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:ro",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", "/var/run/ddl:/run/ddl",
		"-p", fmt.Sprintf("%d:7123", port),
	}

	// Forward env vars to daemon container
	for _, envVar := range []string{
		"DDL_ANTHROPIC_API_KEY", "DDL_OPENAI_API_KEY",
		"DDL_OLLAMA_URL", "DDL_OLLAMA_MODELS", "DDL_OLLAMA_MAX_QUEUE",
		"DDL_OLLAMA_TIMEOUT", "DDL_OLLAMA_DEFAULT_BID",
		"DDL_ENABLE_OPENAI", "DDL_ENABLE_ANTHROPIC", "DDL_ENABLE_OLLAMA",
	} {
		if val := os.Getenv(envVar); val != "" {
			runArgs = append(runArgs, "-e", envVar+"="+val)
		}
	}

	runArgs = append(runArgs, daemonImageName,
		"-db", "/data/ddl.db",
		"-sock", "/run/ddl/ddl.sock",
	)
	out, err := dockerOutput(runArgs...)
	if err != nil {
		return fmt.Errorf("docker run failed: %w", err)
	}
	containerID := strings.TrimSpace(out)
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}

	// Wait for readiness (up to 3 seconds)
	url := fmt.Sprintf("http://localhost:%d/containers", port)
	ready := false
	for i := 0; i < 6; i++ {
		time.Sleep(500 * time.Millisecond)
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if _, serr := os.Stat("/var/run/ddl/ddl.sock"); serr == nil {
				ready = true
				break
			}
			ready = true
			break
		}
	}

	if ready {
		fmt.Printf("ddl-daemon started (container %s)\n", containerID)
		runOnRestartHooks()
	} else {
		fmt.Printf("ddl-daemon started (container %s) but not yet responding on port %d\n", containerID, port)
	}
	return nil
}

func daemonRestartCmd() *cobra.Command {
	var forceBuild bool
	var port int

	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon with minimal downtime",
		Long: `Restart the ddld daemon container. With --build, the image is rebuilt
while the current daemon is still running, then the old container is
swapped for the new one. This minimizes downtime to just the container
swap (~1-2 seconds) instead of including the full build (~30 seconds).

Without --build, the existing image is reused (container restart only).

Examples:
  ddl daemon restart          # restart with current image
  ddl daemon restart --build  # rebuild image first, then swap`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Build first while old daemon keeps running
			if forceBuild {
				if err := buildImage(); err != nil {
					return err
				}
			}

			// Now stop and swap
			state, _ := inspectContainerState(daemonContainerName)
			if state != "" {
				if state == "running" {
					if _, err := dockerOutput("stop", daemonContainerName); err != nil {
						return fmt.Errorf("docker stop failed: %w", err)
					}
				}
				dockerOutput("rm", "-f", daemonContainerName)
			}

			return startDaemonContainer(port)
		},
	}

	cmd.Flags().BoolVar(&forceBuild, "build", false, "Rebuild the ddld image before restarting")
	cmd.Flags().IntVar(&port, "port", 7123, "Host port to expose the API on")

	return cmd
}

func daemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show ddld daemon status",
		Long:  `Show whether the ddl-daemon container is running and its uptime.`,
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

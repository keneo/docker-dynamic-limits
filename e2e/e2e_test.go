//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

var (
	daemonURL  string
	daemonCmd  *exec.Cmd
	dockerCli  *client.Client
	httpClient = &http.Client{Timeout: 10 * time.Second}
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	// Locate ddld binary
	ddldPath := os.Getenv("DDL_BINARY")
	if ddldPath == "" {
		ddldPath = "/usr/local/bin/ddld"
	}

	// Find a free port
	port, err := freePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find free port: %v\n", err)
		return 1
	}

	// Create temp directory for DB
	tmpDir, err := os.MkdirTemp("", "ddl-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)

	dbPath := tmpDir + "/ddl.db"
	addr := fmt.Sprintf(":%d", port)
	daemonURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	// Start ddld subprocess (disable unix socket so tests use full TCP API)
	daemonCmd = exec.Command(ddldPath, "-addr", addr, "-db", dbPath, "-sock", "")
	daemonCmd.Stdout = os.Stdout
	daemonCmd.Stderr = os.Stderr
	if err := daemonCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start ddld: %v\n", err)
		return 1
	}
	defer func() {
		daemonCmd.Process.Signal(os.Interrupt)
		daemonCmd.Wait()
	}()

	// Wait for ddld to be healthy
	if err := waitForHealth(daemonURL, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "ddld failed to become healthy: %v\n", err)
		return 1
	}
	fmt.Println("ddld is healthy")

	// Init Docker client
	dockerCli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create Docker client: %v\n", err)
		return 1
	}
	defer dockerCli.Close()

	// Verify Docker connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dockerCli.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to ping Docker: %v\n", err)
		return 1
	}
	fmt.Println("Docker client connected")

	return m.Run()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func waitForHealth(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/containers")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to become healthy", baseURL)
}

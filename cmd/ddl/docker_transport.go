package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os/exec"
)

// dockerExecTransport implements http.RoundTripper by executing
// curl inside the daemon Docker container via "docker exec".
// This is used on macOS where the Unix socket is not accessible
// from the host due to Docker Desktop running in a VM.
type dockerExecTransport struct {
	containerName string
	socketPath    string
}

func (t *dockerExecTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	args := []string{
		"exec", t.containerName,
		"curl", "-si", "--raw", "--unix-socket", t.socketPath,
		"-X", req.Method,
	}

	for key, vals := range req.Header {
		for _, val := range vals {
			args = append(args, "-H", key+": "+val)
		}
	}

	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		if len(body) > 0 {
			args = append(args, "--data-raw", string(body))
		}
	}

	u := "http://localhost" + req.URL.Path
	if req.URL.RawQuery != "" {
		u += "?" + req.URL.RawQuery
	}
	args = append(args, u)

	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker exec curl: %w: %s", err, stderr.String())
	}

	resp, err := http.ReadResponse(bufio.NewReader(&stdout), req)
	if err != nil {
		return nil, fmt.Errorf("parse curl response: %w\nraw: %s", err, stdout.String())
	}
	return resp, nil
}

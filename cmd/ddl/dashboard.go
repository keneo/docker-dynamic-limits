package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

//go:embed dashboard/*
var dashboardFS embed.FS

var dashboardPidFile = filepath.Join(os.TempDir(), "ddl-dashboard.pid")

func dashboardCmd() *cobra.Command {
	var listen string
	var open bool

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Start the web dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDashboard(listen, open)
		},
	}

	cmd.Flags().StringVar(&listen, "listen", ":7124", "address to listen on")
	cmd.Flags().BoolVar(&open, "open", false, "open browser automatically")

	cmd.AddCommand(dashboardStopCmd())

	return cmd
}

func dashboardStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(dashboardPidFile)
			if err != nil {
				fmt.Println("Dashboard is not running (no PID file)")
				return nil
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				os.Remove(dashboardPidFile)
				fmt.Println("Dashboard is not running (invalid PID file)")
				return nil
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				os.Remove(dashboardPidFile)
				fmt.Println("Dashboard is not running")
				return nil
			}
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				os.Remove(dashboardPidFile)
				fmt.Println("Dashboard is not running")
				return nil
			}
			os.Remove(dashboardPidFile)
			fmt.Println("Dashboard stopped")
			return nil
		},
	}
}

func writePidFile() {
	os.WriteFile(dashboardPidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
}

func removePidFile() {
	os.Remove(dashboardPidFile)
}

func buildDashboardHandler(backendURL string) (http.Handler, error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	if httpClient.Transport != nil {
		proxy.Transport = httpClient.Transport
	}

	subFS, err := fs.Sub(dashboardFS, "dashboard")
	if err != nil {
		return nil, fmt.Errorf("embedded filesystem: %w", err)
	}
	fileServer := http.FileServer(http.FS(subFS))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.URL.RawPath = ""
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	})
	mux.Handle("/", fileServer)

	return mux, nil
}

func runDashboard(listen string, open bool) error {
	handler, err := buildDashboardHandler(apiURL)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listen, err)
	}

	addr := ln.Addr().String()
	dashURL := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	fmt.Printf("Dashboard: %s\n", dashURL)
	fmt.Printf("API proxy: %s -> %s\n", addr, apiURL)

	writePidFile()
	defer removePidFile()

	if open {
		openBrowser(dashURL)
	}

	srv := &http.Server{Handler: handler}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	case "windows":
		cmd = "rundll32"
		url = "url.dll,FileProtocolHandler " + url
	default:
		return
	}
	exec.Command(cmd, url).Start()
}

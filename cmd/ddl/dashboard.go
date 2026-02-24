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
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

//go:embed dashboard/*
var dashboardFS embed.FS

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

	return cmd
}

func buildDashboardHandler(backendURL string) (http.Handler, error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

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

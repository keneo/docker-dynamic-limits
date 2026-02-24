package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardStaticFiles(t *testing.T) {
	// Use a dummy backend URL; static file tests don't hit the proxy
	handler, err := buildDashboardHandler("http://localhost:9999")
	if err != nil {
		t.Fatalf("buildDashboardHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	tests := []struct {
		path        string
		contentType string
		contains    string
	}{
		{"/", "text/html", "<!DOCTYPE html>"},
		{"/index.html", "text/html", "Docker Dynamic Limits"},
		{"/style.css", "text/css", ":root"},
		{"/app.js", "text/javascript", "refresh"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s: status = %d, want 200", tt.path, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, tt.contentType) {
				t.Errorf("GET %s: Content-Type = %q, want to contain %q", tt.path, ct, tt.contentType)
			}

			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tt.contains) {
				t.Errorf("GET %s: body does not contain %q", tt.path, tt.contains)
			}
		})
	}
}

func TestDashboardProxyForwardGET(t *testing.T) {
	// Mock ddld backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers" {
			t.Errorf("backend got path %q, want /containers", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("backend got method %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]string{})
	}))
	defer backend.Close()

	handler, err := buildDashboardHandler(backend.URL)
	if err != nil {
		t.Fatalf("buildDashboardHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/containers")
	if err != nil {
		t.Fatalf("GET /api/containers: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDashboardProxyForwardPUT(t *testing.T) {
	var gotPath, gotMethod, gotBody string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"value": 3600})
	}))
	defer backend.Close()

	handler, err := buildDashboardHandler(backend.URL)
	if err != nil {
		t.Fatalf("buildDashboardHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	reqBody := `{"type":"cpu","value":3600,"operation":"set"}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/containers/abc123/limits", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /api/containers/abc123/limits: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/containers/abc123/limits" {
		t.Errorf("backend path = %q, want /containers/abc123/limits", gotPath)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("backend method = %q, want PUT", gotMethod)
	}
	if gotBody != reqBody {
		t.Errorf("backend body = %q, want %q", gotBody, reqBody)
	}
}

func TestDashboardProxyForwardPOST(t *testing.T) {
	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": "abc123456789"})
	}))
	defer backend.Close()

	handler, err := buildDashboardHandler(backend.URL)
	if err != nil {
		t.Fatalf("buildDashboardHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/register", "application/json",
		strings.NewReader(`{"container_id":"test123"}`))
	if err != nil {
		t.Fatalf("POST /api/register: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/register" {
		t.Errorf("backend path = %q, want /register", gotPath)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDashboardProxyForwardDELETE(t *testing.T) {
	var gotPath, gotMethod string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer backend.Close()

	handler, err := buildDashboardHandler(backend.URL)
	if err != nil {
		t.Fatalf("buildDashboardHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/containers/abc123", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/containers/abc123: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/containers/abc123" {
		t.Errorf("backend path = %q, want /containers/abc123", gotPath)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("backend method = %q, want DELETE", gotMethod)
	}
}

func TestDashboardProxyStripAPIPrefix(t *testing.T) {
	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer backend.Close()

	handler, err := buildDashboardHandler(backend.URL)
	if err != nil {
		t.Fatalf("buildDashboardHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// /api/ alone should map to /
	resp, err := http.Get(ts.URL + "/api/")
	if err != nil {
		t.Fatalf("GET /api/: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/" {
		t.Errorf("backend path = %q, want /", gotPath)
	}
}

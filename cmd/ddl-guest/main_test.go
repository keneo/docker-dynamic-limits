package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveAPIURL_EnvVar(t *testing.T) {
	t.Setenv("DDL_API_URL", "http://custom:9999")
	url, err := resolveAPIURL(nil)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://custom:9999" {
		t.Errorf("got %q, want %q", url, "http://custom:9999")
	}
}

func TestResolveAPIURL_Probe(t *testing.T) {
	t.Setenv("DDL_API_URL", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// First candidate is unreachable, second is our test server
	candidates := []string{"http://127.0.0.1:1", srv.URL}
	url, err := resolveAPIURL(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if url != srv.URL {
		t.Errorf("got %q, want %q", url, srv.URL)
	}
}

func TestResolveAPIURL_NoneReachable(t *testing.T) {
	t.Setenv("DDL_API_URL", "")
	candidates := []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}
	_, err := resolveAPIURL(candidates)
	if err == nil {
		t.Fatal("expected error when no candidates reachable")
	}
}

func TestFetchStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"cpu":330,"ram":134217728},"limits":{"cpu":3600,"ram":536870912}}`)
	}))
	defer srv.Close()

	body, err := fetchStatus(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if body["usage"] == nil {
		t.Fatal("missing usage key")
	}
	if body["limits"] == nil {
		t.Fatal("missing limits key")
	}
}

func TestFetchStatus_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchStatus(srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code: %v", err)
	}
}

func TestPrintStatus(t *testing.T) {
	body := map[string]interface{}{
		"usage": map[string]interface{}{
			"cpu":           json.Number("330"),
			"ram":           json.Number("134217728"),
			"disk":          json.Number("0"),
			"net":           json.Number("52428800"),
			"disk-io-bytes": json.Number("209715200"),
			"disk-io-ops":   json.Number("15000"),
			"spending":      json.Number("150"),
		},
		"limits": map[string]interface{}{
			"cpu":           json.Number("3600"),
			"ram":           json.Number("536870912"),
			"disk":          json.Number("0"),
			"net":           json.Number("1073741824"),
			"disk-io-bytes": json.Number("5368709120"),
			"disk-io-ops":   json.Number("1000000"),
			"spending":      json.Number("1000"),
		},
	}

	var buf bytes.Buffer
	printStatus(&buf, "abc123def456", body)
	output := buf.String()

	// Check header
	if !strings.Contains(output, "Container: abc123def456") {
		t.Errorf("missing container header in output:\n%s", output)
	}

	// Check column headers
	if !strings.Contains(output, "TYPE") || !strings.Contains(output, "USAGE") ||
		!strings.Contains(output, "LIMIT") || !strings.Contains(output, "STATUS") {
		t.Errorf("missing column headers in output:\n%s", output)
	}

	// Check CPU row
	if !strings.Contains(output, "5m30s") {
		t.Errorf("missing CPU usage '5m30s' in output:\n%s", output)
	}
	if !strings.Contains(output, "1h0m0s") {
		t.Errorf("missing CPU limit '1h0m0s' in output:\n%s", output)
	}

	// Check RAM row
	if !strings.Contains(output, "128.0M") {
		t.Errorf("missing RAM usage '128.0M' in output:\n%s", output)
	}
	if !strings.Contains(output, "512.0M") {
		t.Errorf("missing RAM limit '512.0M' in output:\n%s", output)
	}

	// Check spending row
	if !strings.Contains(output, "$1.50") {
		t.Errorf("missing spending usage '$1.50' in output:\n%s", output)
	}
	if !strings.Contains(output, "$10.00") {
		t.Errorf("missing spending limit '$10.00' in output:\n%s", output)
	}

	// Check percentage
	if !strings.Contains(output, "9.2%") {
		t.Errorf("missing CPU percentage in output:\n%s", output)
	}

	// Check no-limit shows dash
	if !strings.HasPrefix(getLineForType(output, "disk"), "disk") {
		t.Errorf("missing disk row in output:\n%s", output)
	}
}

func TestPrintStatus_NoLimitShowsDash(t *testing.T) {
	body := map[string]interface{}{
		"usage": map[string]interface{}{
			"cpu":           json.Number("100"),
			"ram":           json.Number("0"),
			"disk":          json.Number("0"),
			"net":           json.Number("0"),
			"disk-io-bytes": json.Number("0"),
			"disk-io-ops":   json.Number("0"),
			"spending":      json.Number("0"),
		},
		"limits": map[string]interface{}{
			"cpu":           json.Number("0"),
			"ram":           json.Number("0"),
			"disk":          json.Number("0"),
			"net":           json.Number("0"),
			"disk-io-bytes": json.Number("0"),
			"disk-io-ops":   json.Number("0"),
			"spending":      json.Number("0"),
		},
	}

	var buf bytes.Buffer
	printStatus(&buf, "test123", body)
	output := buf.String()

	cpuLine := getLineForType(output, "cpu")
	// CPU has usage=100 but limit=0, so limit and status should be "-"
	parts := strings.Fields(cpuLine)
	// parts: [cpu, 1m40s, -, -]
	if len(parts) < 4 {
		t.Fatalf("unexpected cpu line format: %q", cpuLine)
	}
	if parts[2] != "-" {
		t.Errorf("expected limit '-', got %q", parts[2])
	}
	if parts[3] != "-" {
		t.Errorf("expected status '-', got %q", parts[3])
	}
}

func TestPrintJSON(t *testing.T) {
	body := map[string]interface{}{
		"usage":  map[string]interface{}{"cpu": json.Number("100")},
		"limits": map[string]interface{}{"cpu": json.Number("3600")},
	}

	var buf bytes.Buffer
	printJSON(&buf, body)
	output := buf.String()

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, output)
	}
	if parsed["usage"] == nil || parsed["limits"] == nil {
		t.Errorf("missing keys in JSON output: %s", output)
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		limitType string
		value     int64
		want      string
	}{
		{"cpu", 0, "-"},
		{"cpu", 30, "30s"},
		{"cpu", 90, "1m30s"},
		{"cpu", 3661, "1h1m1s"},
		{"ram", 0, "-"},
		{"ram", 1024, "1.0K"},
		{"ram", 1048576, "1.0M"},
		{"ram", 1073741824, "1.0G"},
		{"spending", 0, "-"},
		{"spending", 150, "$1.50"},
		{"spending", 1000, "$10.00"},
		{"disk-io-ops", 0, "-"},
		{"disk-io-ops", 15000, "15000"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%d", tt.limitType, tt.value), func(t *testing.T) {
			got := formatValue(tt.limitType, tt.value)
			if got != tt.want {
				t.Errorf("formatValue(%q, %d) = %q, want %q", tt.limitType, tt.value, got, tt.want)
			}
		})
	}
}

func TestGetJSONInt64(t *testing.T) {
	m := map[string]interface{}{
		"float":  float64(42),
		"number": json.Number("100"),
	}

	if got := getJSONInt64(m, "float"); got != 42 {
		t.Errorf("float: got %d, want 42", got)
	}
	if got := getJSONInt64(m, "number"); got != 100 {
		t.Errorf("number: got %d, want 100", got)
	}
	if got := getJSONInt64(m, "missing"); got != 0 {
		t.Errorf("missing: got %d, want 0", got)
	}
	if got := getJSONInt64(nil, "key"); got != 0 {
		t.Errorf("nil map: got %d, want 0", got)
	}
}

// getLineForType extracts the line from output that starts with the given type name.
func getLineForType(output, typeName string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, typeName) {
			return trimmed
		}
	}
	return ""
}

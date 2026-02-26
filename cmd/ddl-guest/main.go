package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

var defaultCandidates = []string{
	"http://host.docker.internal:7123",
	"http://172.17.0.1:7123",
}

func main() {
	jsonFlag := flag.Bool("json", false, "output raw JSON")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `ddl-guest — query resource limits and usage from inside a container.

Automatically discovers the ddld daemon by trying common Docker bridge
addresses (host.docker.internal:7123, 172.17.0.1:7123). Override with
the DDL_API_URL environment variable.

Usage:
  ddl-guest          formatted table output
  ddl-guest -json    raw JSON output

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	apiURL, err := resolveAPIURL(defaultCandidates)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	body, err := fetchStatus(apiURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *jsonFlag {
		printJSON(os.Stdout, body)
	} else {
		hostname, _ := os.Hostname()
		printStatus(os.Stdout, hostname, body)
	}
}

func resolveAPIURL(candidates []string) (string, error) {
	if url := os.Getenv("DDL_API_URL"); url != "" {
		return url, nil
	}

	client := &http.Client{Timeout: 1 * time.Second}
	for _, candidate := range candidates {
		resp, err := client.Get(candidate + "/containers")
		if err == nil {
			resp.Body.Close()
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot reach ddld API; set DDL_API_URL or ensure ddld is running")
}

func fetchStatus(apiURL string) (map[string]interface{}, error) {
	resp, err := http.Get(apiURL + "/usage")
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse API response: %w", err)
	}
	return result, nil
}

func printJSON(w io.Writer, body map[string]interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(body)
}

func printStatus(w io.Writer, containerID string, body map[string]interface{}) {
	fmt.Fprintf(w, "Container: %s\n\n", containerID)

	usageMap, _ := body["usage"].(map[string]interface{})
	limitsMap, _ := body["limits"].(map[string]interface{})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TYPE\tUSAGE\tLIMIT\tSTATUS\n")

	types := []string{"cpu", "ram", "disk", "net", "disk-io-bytes", "disk-io-ops", "spending"}
	for _, t := range types {
		usage := getJSONInt64(usageMap, t)
		limit := getJSONInt64(limitsMap, t)

		usageStr := formatValue(t, usage)
		limitStr := formatValue(t, limit)

		var statusStr string
		if limit > 0 {
			pct := float64(usage) / float64(limit) * 100
			statusStr = fmt.Sprintf("%.1f%%", pct)
		} else {
			statusStr = "-"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t, usageStr, limitStr, statusStr)
	}
	tw.Flush()
}

func getJSONInt64(m map[string]interface{}, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch v := v.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// formatValue converts an int64 value to a human-readable string based on limit type.
func formatValue(limitType string, v int64) string {
	if v == 0 {
		return "-"
	}
	switch limitType {
	case "cpu":
		if v >= 3600 {
			return fmt.Sprintf("%dh%dm%ds", v/3600, (v%3600)/60, v%60)
		}
		if v >= 60 {
			return fmt.Sprintf("%dm%ds", v/60, v%60)
		}
		return fmt.Sprintf("%ds", v)
	case "ram", "disk", "net", "disk-io-bytes":
		return formatBytesHuman(v)
	case "spending":
		return fmt.Sprintf("$%.2f", float64(v)/100)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func formatBytesHuman(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.1fT", float64(b)/float64(tb))
	case b >= gb:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1fK", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

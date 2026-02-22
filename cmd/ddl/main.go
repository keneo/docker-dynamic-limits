package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var apiURL = "http://localhost:7123"

func init() {
	if u := os.Getenv("DDL_API_URL"); u != "" {
		apiURL = u
	}
}

func main() {
	root := &cobra.Command{
		Use:   "ddl",
		Short: "Docker Dynamic Limits - manage resource limits on Docker containers",
	}

	root.PersistentFlags().StringVar(&apiURL, "api", apiURL, "ddld API URL")

	root.AddCommand(registerCmd())
	root.AddCommand(limitsCmd())
	root.AddCommand(usageCmd())
	root.AddCommand(cloneCmd())
	root.AddCommand(lsCmd())
	root.AddCommand(removeCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func registerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <container_id>",
		Short: "Start managing a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{"container_id": args[0]}
			resp, err := apiPost("/register", body)
			if err != nil {
				return err
			}
			fmt.Printf("Registered container: %s\n", resp["id"])
			if name, ok := resp["name"]; ok {
				fmt.Printf("Name: %s\n", name)
			}
			return nil
		},
	}
}

func limitsCmd() *cobra.Command {
	limits := &cobra.Command{
		Use:   "limits",
		Short: "Manage container limits",
	}

	limits.AddCommand(&cobra.Command{
		Use:   "set <container> <type> <value>",
		Short: "Set a limit (cpu 3600s, ram 512m, net 1g, disk 10g, disk-io-bytes 5g, disk-io-ops 1000000, spending 10.00)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setLimit(args[0], args[1], args[2], "set")
		},
	})

	limits.AddCommand(&cobra.Command{
		Use:   "increase <container> <type> <value>",
		Short: "Increase a limit by the given amount",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setLimit(args[0], args[1], args[2], "increase")
		},
	})

	limits.AddCommand(&cobra.Command{
		Use:   "decrease <container> <type> <value>",
		Short: "Decrease a limit by the given amount",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setLimit(args[0], args[1], args[2], "decrease")
		},
	})

	limits.AddCommand(&cobra.Command{
		Use:   "get <container>",
		Short: "Show all limits for a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet(fmt.Sprintf("/containers/%s/limits", args[0]))
			if err != nil {
				return err
			}
			printLimitsOrUsage(resp, "Limit")
			return nil
		},
	})

	return limits
}

func usageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage <container>",
		Short: "Show current usage for a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get both limits and usage for a combined view
			usage, err := apiGet(fmt.Sprintf("/containers/%s/usage", args[0]))
			if err != nil {
				return err
			}
			limits, _ := apiGet(fmt.Sprintf("/containers/%s/limits", args[0]))

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "TYPE\tUSAGE\tLIMIT\tSTATUS\n")
			types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending"}
			for _, t := range types {
				u := getJSONFloat(usage, t)
				l := getJSONFloat(limits, t)
				status := "-"
				if l > 0 {
					pct := u / l * 100
					status = fmt.Sprintf("%.1f%%", pct)
					if u >= l {
						status += " ENFORCED"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t, formatValue(t, int64(u)), formatValue(t, int64(l)), status)
			}
			w.Flush()
			return nil
		},
	}
}

func cloneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clone <container> [new-name]",
		Short: "Clone a container with the same limits",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{}
			if len(args) > 1 {
				body["name"] = args[1]
			}
			resp, err := apiPostPath(fmt.Sprintf("/containers/%s/clone", args[0]), body)
			if err != nil {
				return err
			}
			fmt.Printf("Cloned container: %s\n", resp["id"])
			if name, ok := resp["name"]; ok {
				fmt.Printf("Name: %s\n", name)
			}
			return nil
		},
	}
}

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List managed containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(apiURL + "/containers")
			if err != nil {
				return fmt.Errorf("connect to ddld: %w", err)
			}
			defer resp.Body.Close()

			var containers []map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tNAME\tLIMITS\tENFORCED\n")
			for _, c := range containers {
				ctr := c["container"].(map[string]interface{})
				id := ctr["id"].(string)
				name := ctr["name"].(string)
				limits := c["limits"].(map[string]interface{})
				enforced := c["enforced"].(map[string]interface{})

				limitCount := len(limits)
				enforcedCount := 0
				for _, v := range enforced {
					if v.(bool) {
						enforcedCount++
					}
				}

				fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", id, name, limitCount, enforcedCount)
			}
			w.Flush()
			return nil
		},
	}
}

func removeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <container>",
		Short: "Stop managing a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := http.NewRequest(http.MethodDelete, apiURL+"/containers/"+args[0], nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("connect to ddld: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return readAPIError(resp)
			}
			fmt.Println("Container removed from management")
			return nil
		},
	}
}

// --- Helpers ---

func setLimit(container, limitType, valueStr, operation string) error {
	value, err := parseValue(limitType, valueStr)
	if err != nil {
		return fmt.Errorf("invalid value %q for %s: %w", valueStr, limitType, err)
	}

	body := map[string]interface{}{
		"type":      limitType,
		"value":     value,
		"operation": operation,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPut, apiURL+"/containers/"+container+"/limits", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to ddld: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	newVal := int64(result["value"].(float64))
	fmt.Printf("%s %s limit %sd to %s\n", container, limitType, operation, formatValue(limitType, newVal))
	return nil
}

// parseValue converts a human-readable value string to the internal int64 representation.
func parseValue(limitType, s string) (int64, error) {
	s = strings.TrimSpace(s)

	switch limitType {
	case "cpu":
		// Accept "3600s", "60m", "1h" for seconds
		return parseDuration(s)
	case "ram", "disk", "net", "disk-io-bytes":
		// Accept "512m", "1g", "100k" for bytes
		return parseBytes(s)
	case "disk-io-ops":
		// Plain integer
		return strconv.ParseInt(s, 10, 64)
	case "spending":
		// Accept "10.00" or "10" for USD, store as cents
		return parseCents(s)
	default:
		return strconv.ParseInt(s, 10, 64)
	}
}

func parseDuration(s string) (int64, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty value")
	}
	suffix := s[len(s)-1]
	numStr := s[:len(s)-1]

	switch suffix {
	case 's':
		return strconv.ParseInt(numStr, 10, 64)
	case 'm':
		n, err := strconv.ParseInt(numStr, 10, 64)
		return n * 60, err
	case 'h':
		n, err := strconv.ParseInt(numStr, 10, 64)
		return n * 3600, err
	default:
		// Try plain number as seconds
		return strconv.ParseInt(s, 10, 64)
	}
}

func parseBytes(s string) (int64, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty value")
	}
	suffix := strings.ToLower(s[len(s)-1:])
	numStr := s[:len(s)-1]

	var multiplier int64 = 1
	switch suffix {
	case "k":
		multiplier = 1024
	case "m":
		multiplier = 1024 * 1024
	case "g":
		multiplier = 1024 * 1024 * 1024
	case "t":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		numStr = s // no suffix, treat as bytes
	}

	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		// Try float (e.g., "1.5g")
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, err
		}
		return int64(f * float64(multiplier)), nil
	}
	return n * multiplier, nil
}

func parseCents(s string) (int64, error) {
	// Parse as float dollars, convert to cents
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(f * 100), nil
}

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

func apiGet(path string) (map[string]interface{}, error) {
	resp, err := http.Get(apiURL + path)
	if err != nil {
		return nil, fmt.Errorf("connect to ddld: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func apiPost(path string, body interface{}) (map[string]interface{}, error) {
	return apiPostPath(path, body)
}

func apiPostPath(path string, body interface{}) (map[string]interface{}, error) {
	data, _ := json.Marshal(body)
	resp, err := http.Post(apiURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("connect to ddld: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp map[string]string
	if json.Unmarshal(body, &errResp) == nil {
		if msg, ok := errResp["error"]; ok {
			return fmt.Errorf("API error: %s", msg)
		}
	}
	return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
}

func getJSONFloat(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		switch v := v.(type) {
		case float64:
			return v
		case json.Number:
			f, _ := v.Float64()
			return f
		}
	}
	return 0
}

func printLimitsOrUsage(m map[string]interface{}, label string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "TYPE\t%s\n", strings.ToUpper(label))
	types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending"}
	for _, t := range types {
		v := int64(getJSONFloat(m, t))
		fmt.Fprintf(w, "%s\t%s\n", t, formatValue(t, v))
	}
	w.Flush()
}

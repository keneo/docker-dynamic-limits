package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	apiURL     = "http://localhost:7123"
	sockPath   string
	httpClient = http.DefaultClient
)

func init() {
	if u := os.Getenv("DDL_API_URL"); u != "" {
		apiURL = u
	}
	if s := os.Getenv("DDL_SOCK"); s != "" {
		sockPath = s
	}
}

func setupUnixClient(path string) {
	httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
	apiURL = "http://localhost" // host is ignored, transport dials socket
}

func setupDockerExecClient() {
	httpClient = &http.Client{
		Transport: &dockerExecTransport{
			containerName: daemonContainerName,
			socketPath:    "/run/ddl/ddl.sock",
		},
	}
	apiURL = "http://localhost"
}

func main() {
	root := &cobra.Command{
		Use:   "ddl",
		Short: "Docker Dynamic Limits - manage resource limits on Docker containers",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Determine socket path: explicit flag/env > auto-detect > docker exec fallback > TCP
			if sockPath != "" {
				setupUnixClient(sockPath)
				return
			}
			// Auto-detect default socket location
			const defaultSock = "/var/run/ddl/ddl.sock"
			if _, err := os.Stat(defaultSock); err == nil {
				setupUnixClient(defaultSock)
				return
			}
			// Socket not available locally (e.g. macOS Docker Desktop);
			// fall back to docker exec if the daemon container is running
			// and the user hasn't explicitly set --api.
			apiExplicit := cmd.Root().PersistentFlags().Changed("api")
			if !apiExplicit {
				if state, _ := inspectContainerState(daemonContainerName); state == "running" {
					setupDockerExecClient()
				}
			}
		},
	}

	root.PersistentFlags().StringVar(&apiURL, "api", apiURL, "ddld API URL")
	root.PersistentFlags().StringVar(&sockPath, "sock", sockPath, "ddld unix socket path (overrides --api)")

	root.AddCommand(registerCmd())
	root.AddCommand(limitsCmd())
	root.AddCommand(usageCmd())
	root.AddCommand(cloneCmd())
	root.AddCommand(lsCmd())
	root.AddCommand(removeCmd())
	root.AddCommand(dashboardCmd())
	root.AddCommand(daemonCmd())

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
			types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending",
				"ram-usage-bsec", "disk-usage-bsec", "ram-request-bsec", "disk-request-bsec"}
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
			resp, err := httpClient.Get(apiURL + "/containers")
			if err != nil {
				return wrapConnErr(err)
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
			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
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

	resp, err := httpClient.Do(req)
	if err != nil {
		return wrapConnErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	newVal := int64(result["value"].(float64))
	verb := operation + "d"
	if operation == "set" {
		verb = "set"
	} else if operation == "increase" {
		verb = "increased"
	} else if operation == "decrease" {
		verb = "decreased"
	}
	fmt.Printf("%s %s limit %s to %s\n", container, limitType, verb, formatValue(limitType, newVal))
	return nil
}


func apiGet(path string) (map[string]interface{}, error) {
	resp, err := httpClient.Get(apiURL + path)
	if err != nil {
		return nil, wrapConnErr(err)
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
	resp, err := httpClient.Post(apiURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, wrapConnErr(err)
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

func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection reset") {
		return fmt.Errorf("connect to ddld: %w\nHint: run \"ddl daemon start\" to start the daemon", err)
	}
	return fmt.Errorf("connect to ddld: %w", err)
}


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
	jsonOutput bool
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
		Long: `ddl manages dynamic resource limits on Docker containers.

Set, monitor, and enforce cumulative limits on CPU time, RAM, disk,
network, I/O, and LLM API spending per container — with automatic
enforcement and in-container self-querying.

Getting started:
  ddl daemon start        Start the ddld daemon container
  ddl register <id>       Register a Docker container
  ddl limits set <c> cpu 1h   Set a 1-hour CPU limit
  ddl usage <c>           Show usage vs limits
  ddl usage-all           Show usage for all containers
  ddl ls                  List all managed containers

All commands support --json for machine-readable output:
  ddl ls --json
  ddl usage <c> --json
  ddl usage-all --json

Environment variables:
  DDL_API_URL               Override the daemon API URL (default http://localhost:7123)
  DDL_SOCK                  Override the unix socket path
  DDL_ANTHROPIC_API_KEY     Anthropic API key for spending proxy relay
  DDL_OPENAI_API_KEY        OpenAI API key for spending proxy relay
  DDL_OLLAMA_URL            Ollama server URL (enables Ollama inference proxy)
  DDL_OLLAMA_MODELS         Comma-separated allowed models (e.g. llama3.2:3b,qwen3:8b)
  DDL_OLLAMA_MAX_QUEUE      Max queue size (default 50)
  DDL_OLLAMA_TIMEOUT        Request timeout (default 120s)
  DDL_OLLAMA_DEFAULT_BID    Default bid in milli-cents per wall-second (default 0)
  DDL_ENABLE_OPENAI         Enable/disable OpenAI proxy (true/false)
  DDL_ENABLE_ANTHROPIC      Enable/disable Anthropic proxy (true/false)
  DDL_ENABLE_OLLAMA         Enable/disable Ollama proxy (true/false)`,
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
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	root.PersistentFlags().StringVar(&segmentScope, "segment", "", "Scope operations to a segment (overrides DDL_SEGMENT and persisted scope)")

	cobra.OnInitialize(initScope)

	root.AddCommand(registerCmd())
	root.AddCommand(limitsCmd())
	root.AddCommand(usageCmd())
	root.AddCommand(usageAllCmd())
	root.AddCommand(usageHostCmd())
	root.AddCommand(usageGlobalCmd())
	root.AddCommand(cloneCmd())
	root.AddCommand(lsCmd())
	root.AddCommand(removeCmd())
	root.AddCommand(eventsCmd())
	root.AddCommand(dashboardCmd())
	root.AddCommand(configCmd())
	root.AddCommand(daemonCmd())
	root.AddCommand(logsCmd())
	root.AddCommand(hooksCmd())
	root.AddCommand(segmentsCmd())
	root.AddCommand(scopeCmd())
	root.AddCommand(freezeCmd())
	root.AddCommand(unfreezeCmd())
	root.AddCommand(freezeAllCmd())
	root.AddCommand(unfreezeAllCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func printJSON(v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func registerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <container_id>",
		Short: "Start managing a container",
		Long: `Register a Docker container for resource limit management.

The container ID can be a full ID, short ID, or container name.
Once registered, you can set limits, monitor usage, and enforce budgets.

If the daemon has API keys configured, the response includes a proxy_addr
field for routing LLM API calls through the spending proxy.

Examples:
  ddl register abc123def456
  ddl register my-container`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{"container_id": args[0]}
			resp, err := apiPost("/register", body)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
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
		Long: `Manage resource limits for a container.

Subcommands:
  set        Set a limit to an absolute value
  increase   Increase a limit by a given amount
  decrease   Decrease a limit by a given amount
  get        Show all current limits`,
	}

	limits.AddCommand(&cobra.Command{
		Use:   "set <container> <type> <value>",
		Short: "Set a limit to an absolute value",
		Long: `Set a resource limit to an absolute value.

Limit types and value formats:
  cpu             CPU time: 3600s, 60m, 1h
  ram             Memory: 512m, 1g, 2g
  disk            Disk space: 1g, 10g
  net             Network transfer: 100m, 1g
  disk-io-bytes   I/O bytes: 1g, 5g
  disk-io-ops     I/O operations: 1000000
  spending        USD budget: 10.00 (dollars), stored as milli-cents
  ram-usage-bsec      Cumulative RAM byte-seconds: 100g, 1t
  disk-usage-bsec     Cumulative disk byte-seconds: 500g, 1t
  ram-request-bsec    Cumulative RAM reservation byte-seconds: 1t
  disk-request-bsec   Cumulative disk reservation byte-seconds: 1t

Examples:
  ddl limits set my-container cpu 1h
  ddl limits set my-container ram 512m
  ddl limits set my-container spending 5.00
  ddl limits set my-container disk-io-bytes 5g`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setLimit(args[0], args[1], args[2], "set")
		},
	})

	limits.AddCommand(&cobra.Command{
		Use:   "increase <container> <type> <value>",
		Short: "Increase a limit by the given amount",
		Long: `Increase a resource limit by the given amount.

Uses the same value formats as "limits set". The value is added
to the current limit.

Examples:
  ddl limits increase my-container cpu 30m
  ddl limits increase my-container ram 256m
  ddl limits increase my-container spending 5.00`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setLimit(args[0], args[1], args[2], "increase")
		},
	})

	limits.AddCommand(&cobra.Command{
		Use:   "decrease <container> <type> <value>",
		Short: "Decrease a limit by the given amount",
		Long: `Decrease a resource limit by the given amount.

Uses the same value formats as "limits set". The value is subtracted
from the current limit.

Examples:
  ddl limits decrease my-container ram 128m
  ddl limits decrease my-container spending 2.50`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setLimit(args[0], args[1], args[2], "decrease")
		},
	})

	limits.AddCommand(&cobra.Command{
		Use:   "get <container>",
		Short: "Show all limits for a container",
		Long: `Display all configured limits for a container.

Shows a table of limit types and their current values.

Examples:
  ddl limits get my-container
  ddl limits get abc123def456`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet(fmt.Sprintf("/containers/%s/limits", args[0]))
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			printLimitsOrUsage(resp, "Limit")
			return nil
		},
	})

	// Host/global limit commands (global kept as aliases for backward compat)
	setHostCmd := &cobra.Command{
		Use:   "set-host <type> <value>",
		Short: "Set a host limit to an absolute value",
		Long: `Set a host-level resource limit that applies to the sum of all containers.

Uses the same value formats as "limits set".

Examples:
  ddl limits set-host cpu 24h
  ddl limits set-host spending 100.00`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setGlobalLimit(args[0], args[1], "set")
		},
	}
	setGlobalAlias := &cobra.Command{
		Use:    "set-global <type> <value>",
		Short:  "Alias for set-host",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE:   setHostCmd.RunE,
	}

	increaseHostCmd := &cobra.Command{
		Use:   "increase-host <type> <value>",
		Short: "Increase a host limit by the given amount",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setGlobalLimit(args[0], args[1], "increase")
		},
	}
	increaseGlobalAlias := &cobra.Command{
		Use:    "increase-global <type> <value>",
		Short:  "Alias for increase-host",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE:   increaseHostCmd.RunE,
	}

	decreaseHostCmd := &cobra.Command{
		Use:   "decrease-host <type> <value>",
		Short: "Decrease a host limit by the given amount",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setGlobalLimit(args[0], args[1], "decrease")
		},
	}
	decreaseGlobalAlias := &cobra.Command{
		Use:    "decrease-global <type> <value>",
		Short:  "Alias for decrease-host",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE:   decreaseHostCmd.RunE,
	}

	getHostLimitsRunE := func(cmd *cobra.Command, args []string) error {
		resp, err := apiGet("/host-limits")
		if err != nil {
			return err
		}
		if jsonOutput {
			return printJSON(resp)
		}
		printLimitsOrUsage(resp, "Host Limit")
		return nil
	}
	getHostCmd := &cobra.Command{
		Use:   "get-host",
		Short: "Show all host limits",
		RunE:  getHostLimitsRunE,
	}
	getGlobalAlias := &cobra.Command{
		Use:    "get-global",
		Short:  "Alias for get-host",
		Hidden: true,
		RunE:   getHostLimitsRunE,
	}

	limits.AddCommand(setHostCmd, setGlobalAlias)
	limits.AddCommand(increaseHostCmd, increaseGlobalAlias)
	limits.AddCommand(decreaseHostCmd, decreaseGlobalAlias)
	limits.AddCommand(getHostCmd, getGlobalAlias)

	return limits
}

func usageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage <container>",
		Short: "Show current usage for a container",
		Long: `Show current resource usage for a container compared to its limits.

Displays a table with usage, limit, and percentage for each resource type.
Resources that have exceeded their limit are marked as ENFORCED.

Examples:
  ddl usage my-container`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get both limits and usage for a combined view
			usage, err := apiGet(fmt.Sprintf("/containers/%s/usage", args[0]))
			if err != nil {
				return err
			}
			limits, _ := apiGet(fmt.Sprintf("/containers/%s/limits", args[0]))

			if jsonOutput {
				types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending",
					"ram-usage-bsec", "disk-usage-bsec", "ram-request-bsec", "disk-request-bsec"}
				statusMap := map[string]interface{}{}
				for _, t := range types {
					u := getJSONFloat(usage, t)
					l := getJSONFloat(limits, t)
					entry := map[string]interface{}{"enforced": false}
					if l > 0 {
						entry["percentage"] = u / l * 100
						entry["enforced"] = u >= l
					}
					statusMap[t] = entry
				}
				return printJSON(map[string]interface{}{
					"usage":  usage,
					"limits": limits,
					"status": statusMap,
				})
			}

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
		Long: `Clone a Docker container, copying all its resource limits.

Creates a new container from the same image with identical limit
configuration. Optionally provide a name for the new container.

Examples:
  ddl clone my-container
  ddl clone my-container my-container-2`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{}
			if len(args) > 1 {
				body["name"] = args[1]
			}
			resp, err := apiPostPath(fmt.Sprintf("/containers/%s/clone", args[0]), body)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
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
		Long: `List all containers managed by ddld.

Shows a table with container ID, name, number of configured limits,
and number of currently enforced limits.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := fetchContainers()
			if err != nil {
				return err
			}

			if jsonOutput {
				return printJSON(containers)
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
		Use:     "remove <container>",
		Aliases: []string{"rm"},
		Short:   "Stop managing a container",
		Long: `Remove a container from ddld management.

Stops tracking limits and usage for the container. The container
itself is not stopped or deleted.

Examples:
  ddl remove my-container`,
		Args: cobra.ExactArgs(1),
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
			if jsonOutput {
				return printJSON(map[string]string{"status": "removed"})
			}
			fmt.Println("Container removed from management")
			return nil
		},
	}
}

func usageAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage-all",
		Short: "Show usage for all managed containers",
		Long: `Show resource usage for all managed containers.

Displays usage, limits, and status for every container in one output.
Equivalent to running "ddl usage" for each container.

Examples:
  ddl usage-all
  ddl usage-all --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := fetchContainers()
			if err != nil {
				return err
			}

			if jsonOutput {
				return printJSON(containers)
			}

			types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending",
				"ram-usage-bsec", "disk-usage-bsec", "ram-request-bsec", "disk-request-bsec"}

			for i, c := range containers {
				ctr := c["container"].(map[string]interface{})
				id := ctr["id"].(string)
				name := ctr["name"].(string)

				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("=== %s (%s) ===\n", name, id)

				usage, _ := c["usage"].(map[string]interface{})
				limits, _ := c["limits"].(map[string]interface{})

				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "TYPE\tUSAGE\tLIMIT\tSTATUS\n")
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
			}
			return nil
		},
	}
}

func freezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "freeze <container>",
		Short: "Freeze a container (pause + suspend byte-second accumulators)",
		Long: `Freeze a container: Docker-pauses the container and suspends all
byte-second accumulator tracking. This is different from enforcement
pause — a frozen container does not consume lifetime.

Suspended accumulators:
  ram-usage-bsec, disk-usage-bsec, ram-request-bsec, disk-request-bsec

If the container is already enforcement-paused, only the frozen flag is
set (the container is already Docker-paused). Enforcement release will
NOT Docker-unpause a frozen container.

Any pending Ollama inference requests for this container are cancelled.

Examples:
  ddl freeze my-container
  ddl freeze creature-ollie`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiPost(fmt.Sprintf("/containers/%s/freeze", args[0]), nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			fmt.Printf("Container %s frozen\n", args[0])
			return nil
		},
	}
}

func unfreezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfreeze <container>",
		Short: "Unfreeze a container (resume byte-second accumulators)",
		Long: `Unfreeze a container: resumes byte-second accumulator tracking and
Docker-unpauses the container — unless enforcement is still holding it
paused (e.g. CPU or disk limit exceeded).

If enforcement is still active, the container stays Docker-paused but
byte-second accumulators resume. A warning is printed.

Unfreeze does NOT override enforcement pause.

Examples:
  ddl unfreeze my-container
  ddl unfreeze creature-ollie`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiPost(fmt.Sprintf("/containers/%s/unfreeze", args[0]), nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			fmt.Printf("Container %s unfrozen\n", args[0])
			if ea, ok := resp["enforcement_active"]; ok && ea == true {
				fmt.Println("Warning: enforcement is still active — container remains Docker-paused")
			}
			return nil
		},
	}
}

func freezeAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "freeze-all",
		Short: "Freeze all managed containers",
		Long: `Freeze all managed containers. Each container is Docker-paused and
its byte-second accumulators are suspended.

Examples:
  ddl freeze-all`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiPost("/freeze-all", nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			if results, ok := resp["results"].([]interface{}); ok {
				for _, r := range results {
					if m, ok := r.(map[string]interface{}); ok {
						name := m["name"]
						if name == nil || name == "" {
							name = m["id"]
						}
						fmt.Printf("%s: %s\n", name, m["status"])
					}
				}
			}
			return nil
		},
	}
}

func unfreezeAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfreeze-all",
		Short: "Unfreeze all managed containers",
		Long: `Unfreeze all managed containers. Byte-second accumulators resume.
Containers that are still enforcement-paused remain Docker-paused.

Examples:
  ddl unfreeze-all`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiPost("/unfreeze-all", nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			if results, ok := resp["results"].([]interface{}); ok {
				for _, r := range results {
					if m, ok := r.(map[string]interface{}); ok {
						name := m["name"]
						if name == nil || name == "" {
							name = m["id"]
						}
						status := m["status"]
						if ea, ok := m["enforcement_active"]; ok && ea == true {
							status = fmt.Sprintf("%s (enforcement still active)", status)
						}
						fmt.Printf("%s: %s\n", name, status)
					}
				}
			}
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

	if jsonOutput {
		return printJSON(result)
	}

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


func setGlobalLimit(limitType, valueStr, operation string) error {
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

	req, err := http.NewRequest(http.MethodPut, apiURL+"/global-limits", bytes.NewReader(data))
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

	if jsonOutput {
		return printJSON(result)
	}

	newVal := int64(result["value"].(float64))
	verb := operation
	switch operation {
	case "set":
		verb = "set"
	case "increase":
		verb = "increased"
	case "decrease":
		verb = "decreased"
	}
	fmt.Printf("host %s limit %s to %s\n", limitType, verb, formatValue(limitType, newVal))
	return nil
}

func usageHostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage-host",
		Short: "Show aggregated usage across all containers vs host limits",
		RunE:  usageHostRunE,
	}
}

func usageGlobalCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "usage-global",
		Short:  "Alias for usage-host",
		Hidden: true,
		RunE:   usageHostRunE,
	}
}

func usageHostRunE(cmd *cobra.Command, args []string) error {
	result, err := fetchContainersResponse()
	if err != nil {
		return err
	}

	if jsonOutput {
		return printJSON(map[string]interface{}{
			"host_limits":     result.HostLimits,
			"host_usage":      result.HostUsage,
			"host_enforced":   result.HostEnforced,
			"global_limits":   result.GlobalLimits,
			"global_usage":    result.GlobalUsage,
			"global_enforced": result.GlobalEnforced,
		})
	}

	types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending",
		"ram-usage-bsec", "disk-usage-bsec", "ram-request-bsec", "disk-request-bsec"}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "TYPE\tUSAGE\tHOST LIMIT\tSTATUS\n")
	for _, t := range types {
		u := getJSONFloat(result.HostUsage, t)
		l := getJSONFloat(result.HostLimits, t)
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

// containersResponse represents the response from GET /containers.
type containersResponse struct {
	Containers     []map[string]interface{} `json:"containers"`
	HostLimits     map[string]interface{}   `json:"host_limits"`
	HostUsage      map[string]interface{}   `json:"host_usage"`
	HostEnforced   map[string]interface{}   `json:"host_enforced"`
	GlobalLimits   map[string]interface{}   `json:"global_limits"`   // backward compat
	GlobalUsage    map[string]interface{}   `json:"global_usage"`    // backward compat
	GlobalEnforced map[string]interface{}   `json:"global_enforced"` // backward compat
}

func fetchContainersResponse() (*containersResponse, error) {
	resp, err := httpClient.Get(apiURL + "/containers")
	if err != nil {
		return nil, wrapConnErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}

	var result containersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func fetchContainers() ([]map[string]interface{}, error) {
	result, err := fetchContainersResponse()
	if err != nil {
		return nil, err
	}
	return result.Containers, nil
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


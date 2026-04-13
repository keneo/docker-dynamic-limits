package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var segmentScope string // set by --segment flag or DDL_SEGMENT env or persisted scope

func initScope() {
	// Priority: --segment flag > DDL_SEGMENT env > persisted scope file
	if segmentScope != "" {
		return // already set by flag
	}
	if s := os.Getenv("DDL_SEGMENT"); s != "" {
		segmentScope = s
		return
	}
	if s, err := readPersistedScope(); err == nil && s != "" {
		segmentScope = s
	}
}

func scopeConfigPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "ddl", "scope")
}

func readPersistedScope() (string, error) {
	data, err := os.ReadFile(scopeConfigPath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writePersistedScope(scope string) error {
	p := scopeConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(scope+"\n"), 0644)
}

func clearPersistedScope() error {
	return os.Remove(scopeConfigPath())
}

func scopeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scope",
		Short: "Show or set the active CLI scope",
		Long: `Manage the CLI scope. When a scope is set, most commands operate
only within that scope (segment or host).

Scope resolution order: --segment flag > DDL_SEGMENT env > persisted scope > host`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if segmentScope != "" {
				fmt.Printf("scope: segment %q\n", segmentScope)
			} else {
				fmt.Println("scope: host (default)")
			}
			// Show how it was set
			if s := os.Getenv("DDL_SEGMENT"); s != "" {
				fmt.Printf("  (from DDL_SEGMENT env)\n")
			} else if s, _ := readPersistedScope(); s != "" {
				fmt.Printf("  (from %s)\n", scopeConfigPath())
			}
			return nil
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "set <segment-id|host>",
		Short: "Set persistent CLI scope",
		Long: `Set the CLI scope. Use a segment ID to scope to a segment,
or "host" to explicitly scope to host (all containers).

Examples:
  ddl scope set prod     # scope to segment "prod"
  ddl scope set host     # scope to host`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := args[0]
			if scope == "host" || scope == "" {
				if err := clearPersistedScope(); err != nil && !os.IsNotExist(err) {
					return err
				}
				fmt.Println("scope set to: host")
				return nil
			}
			if err := writePersistedScope(scope); err != nil {
				return err
			}
			fmt.Printf("scope set to: segment %q\n", scope)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "clear",
		Short: "Clear persistent CLI scope (revert to host)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clearPersistedScope(); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Println("scope cleared (host)")
			return nil
		},
	})

	cmd.AddCommand(scopeListenCmd())
	cmd.AddCommand(scopeListenersCmd())
	cmd.AddCommand(scopeUnlistenCmd())

	return cmd
}

func scopeListenCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "listen <segment-id>",
		Short: "Start a scoped API listener for a segment",
		Long: `Start a scoped API listener inside the daemon container.
The port is exposed on the host (ports 7200-7220 are pre-mapped).

Only containers in the specified segment are visible through this listener.

Examples:
  ddl scope listen prod --port 7200
  ddl scope listen prod --port 7205`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := "segment:" + args[0]
			body, _ := json.Marshal(map[string]interface{}{
				"scope":  scope,
				"listen": fmt.Sprintf(":%d", port),
			})
			req, _ := http.NewRequest(http.MethodPost, apiURL+"/scoped-listeners",
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				return readAPIError(resp)
			}

			var result map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&result)

			if jsonOutput {
				return printJSON(result)
			}
			fmt.Printf("scoped listener started on http://localhost:%d (scope=%s)\n", port, scope)
			fmt.Printf("  id: %s\n", result["id"])
			fmt.Printf("  note: port must be in 7200-7220 range (pre-mapped on daemon start)\n")
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 7200, "TCP port (must be in 7200-7220 range)")
	return cmd
}

func scopeListenersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "listeners",
		Short: "List active scoped listeners",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/scoped-listeners")
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			listeners, ok := resp["listeners"].([]interface{})
			if !ok || len(listeners) == 0 {
				fmt.Println("No scoped listeners.")
				return nil
			}
			for _, l := range listeners {
				sl := l.(map[string]interface{})
				fmt.Printf("  %s  %s  http://localhost%s\n", sl["id"], sl["scope"], sl["listen"])
			}
			return nil
		},
	}
}

func scopeUnlistenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlisten <id>",
		Short: "Stop a scoped listener",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, _ := http.NewRequest(http.MethodDelete, apiURL+"/scoped-listeners/"+args[0], nil)
			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return readAPIError(resp)
			}
			fmt.Printf("scoped listener %s stopped\n", args[0])
			return nil
		},
	}
}


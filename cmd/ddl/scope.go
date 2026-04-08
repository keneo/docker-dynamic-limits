package main

import (
	"fmt"
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

	return cmd
}

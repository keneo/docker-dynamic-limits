package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
)

type hooksFile struct {
	OnRestart []string `json:"on_restart"`
}

func hooksFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ddl", "hooks.json")
}

func loadHooks() (*hooksFile, error) {
	data, err := os.ReadFile(hooksFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &hooksFile{}, nil
		}
		return nil, err
	}
	var h hooksFile
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parse hooks file: %w", err)
	}
	return &h, nil
}

func saveHooks(h *hooksFile) error {
	path := hooksFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func runOnRestartHooks() {
	h, err := loadHooks()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load hooks: %v\n", err)
		return
	}
	if len(h.OnRestart) == 0 {
		return
	}
	fmt.Printf("Running %d on-restart hook(s)...\n", len(h.OnRestart))
	for i, cmd := range h.OnRestart {
		fmt.Printf("  [%d] %s\n", i+1, cmd)
		c := exec.Command("sh", "-c", cmd)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%d] FAILED: %v\n", i+1, err)
		}
	}
}

func hooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Manage daemon restart hooks",
		Long: `Manage shell commands that run automatically after "ddl daemon start".

Hooks are stored locally in ~/.config/ddl/hooks.json and execute on the
host (outside Docker) after the daemon container is ready.

Common use case: attaching Docker networks to the daemon container.

Subcommands:
  add      Add a hook command
  list     List all hooks
  remove   Remove a hook by index`,
	}

	cmd.AddCommand(hooksAddCmd())
	cmd.AddCommand(hooksListCmd())
	cmd.AddCommand(hooksRemoveCmd())

	return cmd
}

func hooksAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <command>",
		Short: "Add an on-restart hook",
		Long: `Add a shell command to run after every "ddl daemon start".

The command runs via "sh -c" on the host after the daemon is ready.

Examples:
  ddl hooks add "docker network connect my-net ddl-daemon"
  ddl hooks add "echo daemon started at $(date)"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := loadHooks()
			if err != nil {
				return err
			}
			h.OnRestart = append(h.OnRestart, args[0])
			if err := saveHooks(h); err != nil {
				return err
			}
			fmt.Printf("Hook added (#%d): %s\n", len(h.OnRestart), args[0])
			return nil
		},
	}
}

func hooksListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all on-restart hooks",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := loadHooks()
			if err != nil {
				return err
			}
			if len(h.OnRestart) == 0 {
				fmt.Println("No hooks configured.")
				fmt.Println("Add one with: ddl hooks add \"<command>\"")
				return nil
			}
			fmt.Println("On-restart hooks:")
			for i, c := range h.OnRestart {
				fmt.Printf("  %d. %s\n", i+1, c)
			}
			return nil
		},
	}
}

func hooksRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <index>",
		Short: "Remove an on-restart hook by index",
		Long: `Remove a hook by its 1-based index (as shown in "ddl hooks list").

Examples:
  ddl hooks remove 1`,
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			idx, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid index: %s", args[0])
			}
			h, err := loadHooks()
			if err != nil {
				return err
			}
			if idx < 1 || idx > len(h.OnRestart) {
				return fmt.Errorf("index %d out of range (1-%d)", idx, len(h.OnRestart))
			}
			removed := h.OnRestart[idx-1]
			h.OnRestart = append(h.OnRestart[:idx-1], h.OnRestart[idx:]...)
			if err := saveHooks(h); err != nil {
				return err
			}
			fmt.Printf("Removed hook #%d: %s\n", idx, removed)
			return nil
		},
	}
}

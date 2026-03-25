package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// configKeyInfo maps CLI key names to JSON field names and types.
type configKeyInfo struct {
	jsonKey  string
	keyType  string // "bool", "string", "int", "duration", "stringlist"
	readOnly bool
}

var configKeys = map[string]configKeyInfo{
	"anthropic-enabled": {jsonKey: "anthropic_enabled", keyType: "bool"},
	"openai-enabled":    {jsonKey: "openai_enabled", keyType: "bool"},
	"ollama-enabled":    {jsonKey: "ollama_enabled", keyType: "bool"},
	"anthropic-key":     {jsonKey: "anthropic_key", keyType: "string"},
	"openai-key":        {jsonKey: "openai_key", keyType: "string"},
	"ollama-url":        {jsonKey: "ollama_url", keyType: "string"},
	"ollama-models":     {jsonKey: "ollama_models", keyType: "stringlist"},
	"ollama-queue-size": {jsonKey: "ollama_queue_size", keyType: "int"},
	"ollama-timeout":    {jsonKey: "ollama_timeout", keyType: "duration"},
	"ollama-default-bid":      {jsonKey: "ollama_default_bid", keyType: "int"},
	"error-webhooks":          {jsonKey: "error_webhooks", keyType: "stringlist"},
	"keep-limits-consistent":  {jsonKey: "keep_limits_consistent", keyType: "bool"},
}

// configDisplayOrder defines the order keys are displayed in "config get".
var configDisplayOrder = []string{
	"anthropic-enabled",
	"openai-enabled",
	"ollama-enabled",
	"anthropic-key",
	"openai-key",
	"ollama-url",
	"ollama-models",
	"ollama-queue-size",
	"ollama-timeout",
	"ollama-default-bid",
	"error-webhooks",
	"keep-limits-consistent",
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage daemon configuration",
		Long: `View and update daemon configuration at runtime.

Subcommands:
  get    Show current configuration
  set    Update a configuration value

Examples:
  ddl config get
  ddl config set anthropic-key sk-ant-xxx
  ddl config set ollama-models "llama3.2:3b,qwen3:8b"
  ddl config set ollama-queue-size 100
  ddl config set openai-enabled false`,
	}

	cmd.AddCommand(configGetCmd())
	cmd.AddCommand(configSetCmd())
	return cmd
}

func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Show current daemon configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/config")
			if err != nil {
				return err
			}

			if jsonOutput {
				return printJSON(resp)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "KEY\tVALUE\n")
			for _, key := range configDisplayOrder {
				info := configKeys[key]
				val := formatConfigValue(key, info, resp)
				fmt.Fprintf(w, "%s\t%s\n", key, val)
			}
			w.Flush()
			return nil
		},
	}
}

func configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Update a configuration value",
		Long: `Update a daemon configuration value at runtime.

Keys:
  anthropic-enabled    bool     Enable/disable Anthropic proxy
  openai-enabled       bool     Enable/disable OpenAI proxy
  ollama-enabled       bool     Enable/disable Ollama proxy
  anthropic-key        string   Anthropic API key
  openai-key           string   OpenAI API key
  ollama-url           string   Ollama server URL
  ollama-models        list     Comma-separated model allowlist
  ollama-queue-size    int      Max queue size
  ollama-timeout       duration Request timeout (e.g. 2m, 120s)
  ollama-default-bid   int      Default bid in milli-cents/wall-sec
  error-webhooks       list     Comma-separated webhook URLs for upstream API errors
  keep-limits-consistent bool   Validate per-container vs global limits consistency

Note: ollama-url changes take effect for the next queued request.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, valueStr := args[0], args[1]

			info, ok := configKeys[key]
			if !ok {
				return fmt.Errorf("unknown config key: %s", key)
			}
			if info.readOnly {
				return fmt.Errorf("%s is read-only (requires daemon restart to change)", key)
			}

			val, err := parseConfigValue(info, valueStr)
			if err != nil {
				return fmt.Errorf("invalid value for %s: %w", key, err)
			}

			body := map[string]interface{}{info.jsonKey: val}
			data, _ := json.Marshal(body)

			req, err := http.NewRequest(http.MethodPut, apiURL+"/config", bytes.NewReader(data))
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

			fmt.Printf("%s updated\n", key)
			return nil
		},
	}
}

func parseConfigValue(info configKeyInfo, s string) (interface{}, error) {
	switch info.keyType {
	case "bool":
		switch strings.ToLower(s) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		default:
			return nil, fmt.Errorf("expected bool (true/false)")
		}
	case "string":
		return s, nil
	case "int":
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "duration":
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, err
		}
		return d.String(), nil
	case "stringlist":
		parts := strings.Split(s, ",")
		var result []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result, nil
	default:
		return s, nil
	}
}

func formatConfigValue(key string, info configKeyInfo, resp map[string]interface{}) string {
	// API keys are masked — use the _key_set fields
	if key == "anthropic-key" {
		if set, ok := resp["anthropic_key_set"].(bool); ok && set {
			return "****"
		}
		return "(not set)"
	}
	if key == "openai-key" {
		if set, ok := resp["openai_key_set"].(bool); ok && set {
			return "****"
		}
		return "(not set)"
	}

	val, ok := resp[info.jsonKey]
	if !ok {
		return "(not configured)"
	}

	switch v := val.(type) {
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return "(none)"
		}
		return strings.Join(parts, ", ")
	case nil:
		return "(not configured)"
	default:
		return fmt.Sprintf("%v", v)
	}
}

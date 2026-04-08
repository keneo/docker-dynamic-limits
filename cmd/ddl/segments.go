package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func segmentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "segments",
		Short: "Manage container segments",
		Long: `Segments group containers with shared limits and enforcement.

A container can belong to at most one segment. Segment limits are enforced
on the sum of usage across all member containers, in addition to host limits.`,
	}

	cmd.AddCommand(segmentsCreateCmd())
	cmd.AddCommand(segmentsListCmd())
	cmd.AddCommand(segmentsDeleteCmd())
	cmd.AddCommand(segmentsShowCmd())
	cmd.AddCommand(segmentsAssignCmd())
	cmd.AddCommand(segmentsUnassignCmd())
	cmd.AddCommand(segmentsLimitsCmd())
	cmd.AddCommand(segmentsUsageCmd())
	cmd.AddCommand(segmentsFreezeAllCmd())
	cmd.AddCommand(segmentsUnfreezeAllCmd())
	cmd.AddCommand(segmentsConfigCmd())

	return cmd
}

func segmentsCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create a new segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if name == "" {
				name = id
			}
			body, _ := json.Marshal(map[string]string{"id": id, "name": name})
			req, _ := http.NewRequest(http.MethodPost, apiURL+"/segments", bytes.NewReader(body))
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
			fmt.Printf("segment %q created\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name (defaults to id)")
	return cmd
}

func segmentsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all segments",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/segments")
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			segments, ok := resp["segments"].([]interface{})
			if !ok || len(segments) == 0 {
				fmt.Println("No segments.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tNAME\tCREATED\n")
			for _, s := range segments {
				seg := s.(map[string]interface{})
				fmt.Fprintf(w, "%s\t%s\t%s\n", seg["id"], seg["name"], seg["created_at"])
			}
			w.Flush()
			return nil
		},
	}
}

func segmentsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, _ := http.NewRequest(http.MethodDelete, apiURL+"/segments/"+args[0], nil)
			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return readAPIError(resp)
			}
			fmt.Printf("segment %q deleted\n", args[0])
			return nil
		},
	}
}

func segmentsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show segment detail (limits, usage, containers)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/segments/" + args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}

			seg := resp["segment"].(map[string]interface{})
			fmt.Printf("Segment: %s (%s)\n", seg["id"], seg["name"])
			fmt.Printf("Containers: %v\n\n", resp["containers"])

			if limits, ok := resp["limits"].(map[string]interface{}); ok && len(limits) > 0 {
				fmt.Println("Limits:")
				for k, v := range limits {
					fmt.Printf("  %s: %s\n", k, formatValue(k, int64(v.(float64))))
				}
			}
			if usage, ok := resp["usage"].(map[string]interface{}); ok && len(usage) > 0 {
				fmt.Println("Usage:")
				for k, v := range usage {
					fmt.Printf("  %s: %s\n", k, formatValue(k, int64(v.(float64))))
				}
			}
			return nil
		},
	}
}

func segmentsAssignCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "assign <container> <segment>",
		Short: "Assign a container to a segment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			containerID := args[0]
			segID := args[1]
			req, _ := http.NewRequest(http.MethodPost,
				apiURL+"/segments/"+segID+"/containers/"+containerID+"/assign", nil)
			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return readAPIError(resp)
			}
			fmt.Printf("container %s assigned to segment %q\n", containerID, segID)
			return nil
		},
	}
}

func segmentsUnassignCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unassign <container> <segment>",
		Short: "Remove a container from a segment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			containerID := args[0]
			segID := args[1]
			req, _ := http.NewRequest(http.MethodPost,
				apiURL+"/segments/"+segID+"/containers/"+containerID+"/unassign", nil)
			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return readAPIError(resp)
			}
			fmt.Printf("container %s removed from segment %q\n", containerID, segID)
			return nil
		},
	}
}

func segmentsLimitsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "Manage segment limits",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "set <segment> <type> <value>",
		Short: "Set a segment limit",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setSegmentLimit(args[0], args[1], args[2], "set")
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "get <segment>",
		Short: "Show segment limits",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/segments/" + args[0] + "/limits")
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			printLimitsOrUsage(resp, "Segment Limit")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "increase <segment> <type> <value>",
		Short: "Increase a segment limit",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setSegmentLimit(args[0], args[1], args[2], "increase")
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "decrease <segment> <type> <value>",
		Short: "Decrease a segment limit",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setSegmentLimit(args[0], args[1], args[2], "decrease")
		},
	})

	return cmd
}

func setSegmentLimit(segID, limitType, rawValue, operation string) error {
	value, err := parseValue(limitType, rawValue)
	if err != nil {
		return fmt.Errorf("invalid value %q for %s: %w", rawValue, limitType, err)
	}

	data, _ := json.Marshal(map[string]interface{}{
		"type":      limitType,
		"value":     value,
		"operation": operation,
	})

	req, _ := http.NewRequest(http.MethodPut,
		apiURL+"/segments/"+segID+"/limits", bytes.NewReader(data))
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
	fmt.Printf("segment %s %s limit %s to %s\n", segID, limitType, verb, formatValue(limitType, newVal))
	return nil
}

func segmentsUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage <segment>",
		Short: "Show aggregated usage for a segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/segments/" + args[0] + "/usage")
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}

			usage := resp["usage"].(map[string]interface{})
			limits := resp["limits"].(map[string]interface{})

			types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending",
				"ram-usage-bsec", "disk-usage-bsec", "ram-request-bsec", "disk-request-bsec"}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "TYPE\tUSAGE\tSEGMENT LIMIT\tSTATUS\n")
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

func segmentsFreezeAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "freeze-all <segment>",
		Short: "Freeze all containers in a segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, _ := http.NewRequest(http.MethodPost,
				apiURL+"/segments/"+args[0]+"/freeze-all", nil)
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
			fmt.Printf("frozen %v containers in segment %q\n", result["count"], args[0])
			return nil
		},
	}
}

func segmentsUnfreezeAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfreeze-all <segment>",
		Short: "Unfreeze all containers in a segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, _ := http.NewRequest(http.MethodPost,
				apiURL+"/segments/"+args[0]+"/unfreeze-all", nil)
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
			fmt.Printf("unfrozen %v containers in segment %q\n", result["count"], args[0])
			return nil
		},
	}
}

func segmentsConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage per-segment configuration",
		Long: `View or set per-segment config overrides.
Segment config inherits from host config. Only explicitly set keys override.

Supported keys: anthropic-enabled, openai-enabled, ollama-enabled,
anthropic-key, openai-key, ollama-models, error-webhooks`,
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "get <segment>",
		Short: "Show effective config for a segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiGet("/segments/" + args[0] + "/config")
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(resp)
			}
			// Show overrides separately
			overrides, _ := resp["_segment_overrides"].(map[string]interface{})
			delete(resp, "_segment_overrides")

			fmt.Printf("Effective config for segment %q:\n", args[0])
			for k, v := range resp {
				marker := ""
				if _, ok := overrides[k]; ok {
					marker = " (override)"
				}
				fmt.Printf("  %s: %v%s\n", k, v, marker)
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set <segment> <key> <value>",
		Short: "Set a config override for a segment",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			segID, key, value := args[0], args[1], args[2]
			body, _ := json.Marshal(map[string]interface{}{key: value})
			req, _ := http.NewRequest(http.MethodPut,
				apiURL+"/segments/"+segID+"/config", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				return wrapConnErr(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return readAPIError(resp)
			}
			fmt.Printf("segment %s config %s updated\n", segID, key)
			return nil
		},
	})

	return cmd
}

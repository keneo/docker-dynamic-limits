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

func segmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "segment",
		Short: "Manage container segments",
		Long: `Segments group containers with shared limits and enforcement.

A container can belong to at most one segment. Segment limits are enforced
on the sum of usage across all member containers, in addition to host limits.

Use "ddl limits set-segment", "ddl usage-segment", "ddl freeze-segment"
for segment limits, usage, and freeze operations.`,
	}

	cmd.AddCommand(segmentCreateCmd())
	cmd.AddCommand(segmentListCmd())
	cmd.AddCommand(segmentDeleteCmd())
	cmd.AddCommand(segmentShowCmd())
	cmd.AddCommand(segmentAssignCmd())
	cmd.AddCommand(segmentUnassignCmd())

	return cmd
}

// Hidden alias for backward compat
func segmentsCmd() *cobra.Command {
	cmd := segmentCmd()
	cmd.Use = "segments"
	cmd.Hidden = true
	return cmd
}

func segmentCreateCmd() *cobra.Command {
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

func segmentListCmd() *cobra.Command {
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

func segmentDeleteCmd() *cobra.Command {
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

func segmentShowCmd() *cobra.Command {
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

func segmentAssignCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "assign <container> [<segment>]",
		Short: "Assign a container to a segment",
		Long: `Assign a container to a segment. If segment is omitted, uses current scope.

Examples:
  ddl segment assign my-container prod
  ddl scope set prod && ddl segment assign my-container`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			containerID := args[0]
			segID := segmentScope
			if len(args) > 1 {
				segID = args[1]
			}
			if segID == "" {
				return fmt.Errorf("segment required (provide as argument or set with 'ddl scope set')")
			}
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

func segmentUnassignCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unassign <container>",
		Short: "Remove a container from its segment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			containerID := args[0]
			// Find which segment the container is in
			cResp, err := apiGet("/containers/" + containerID)
			if err != nil {
				return err
			}
			ctr, ok := cResp["container"].(map[string]interface{})
			if !ok {
				return fmt.Errorf("unexpected response")
			}
			segID, _ := ctr["segment_id"].(string)
			if segID == "" {
				fmt.Printf("container %s is not in any segment\n", containerID)
				return nil
			}
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

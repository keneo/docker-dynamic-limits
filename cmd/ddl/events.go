package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

func eventsCmd() *cobra.Command {
	var containerFilter string
	var typesFilter string
	var rawOutput bool

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream real-time events from the daemon",
		Long: `Subscribe to a live event stream from ddld via WebSocket.

Event types:
  usage_update               Per-container usage snapshot (every ~1s per container)
  limit_change               A limit was set, increased, or decreased
  enforcement_change         Enforcement was applied or released
  global_enforcement_change  A host/global limit was enforced or released (compat alias)
  scope_enforcement_change   A scope (host or segment) limit was enforced or released
  segment_create             A segment was created
  segment_delete             A segment was deleted
  container_assign           A container was assigned to a segment
  container_unassign         A container was removed from a segment
  container_register         A new container was registered
  container_remove           A container was removed from management
  container_frozen           A container was frozen (paused + byte-sec suspended)
  container_unfrozen         A container was unfrozen
  container_killed           A container was killed due to resource limit
  proxy_upstream_error       An upstream LLM API returned an error
  system_sleep               Host sleep detected (VM suspend/resume)
  ollama_enqueue             An Ollama inference request was queued
  ollama_dequeue             An Ollama inference request completed
  ollama_cancel              An Ollama inference request was cancelled
  ollama_bid_change          A container's Ollama bid was changed

Event payloads (visible with --raw):
  All events include: type, container_id, timestamp, data

  usage_update               {usage: {<type>: int}, limits: {<type>: int}, enforced: {<type>: bool}}
  limit_change               {limit_type, old_value, new_value, operation}
  enforcement_change         {limit_type, enforced}
  global_enforcement_change  {limit_type, enforced}
  scope_enforcement_change   {scope, limit_type, enforced}
  segment_create             {segment_id, name}
  segment_delete             {segment_id}
  container_assign           {segment_id}
  container_unassign         {segment_id}
  container_register         {docker_id, name}
  container_remove           {}
  container_frozen           {}
  container_unfrozen         {enforcement_active}
  container_killed           {limit_type, usage_at_kill, limit_at_kill}
  proxy_upstream_error       {host, status_code, error_type, error_message, request_id}
  system_sleep               {duration_seconds}
  ollama_enqueue             {model, bid}
  ollama_dequeue             {model, wall_seconds, cost}
  ollama_cancel              {reason}
  ollama_bid_change          {bid}

Examples:
  ddl events                                    # stream all events
  ddl events --container my-container           # filter by container
  ddl events --types limit_change               # filter by event type
  ddl events --types limit_change,enforcement_change
  ddl events --types ollama_enqueue,ollama_dequeue
  ddl events --raw                              # NDJSON output
  ddl events --raw | jq .                       # pipe to jq
  ddl events --json                             # same as --raw

Limitation: WebSocket is not available via docker exec transport (macOS fallback).
Use --sock or DDL_SOCK to connect via unix socket, or --api to connect via TCP.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Build WebSocket URL
			wsURLStr, err := buildWSURL(containerFilter, typesFilter)
			if err != nil {
				return err
			}

			dialer := websocket.Dialer{}

			// If using a unix socket, configure the dialer
			if sockPath != "" {
				dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				}
			} else {
				// Auto-detect default socket
				const defaultSock = "/var/run/ddl/ddl.sock"
				if _, err := os.Stat(defaultSock); err == nil {
					dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", defaultSock)
					}
					wsURLStr = buildWSURLFrom("ws://localhost", containerFilter, typesFilter)
				} else if apiURL == "http://localhost" {
					// Docker exec fallback — WebSocket doesn't work through docker exec,
					// connect directly to the TCP API at port 7123 instead
					wsURLStr = buildWSURLFrom("ws://localhost:7123", containerFilter, typesFilter)
				}
			}

			conn, _, err := dialer.Dial(wsURLStr, nil)
			if err != nil {
				return fmt.Errorf("connect to event stream: %w\nHint: ensure ddld is running and accessible", err)
			}
			defer conn.Close()

			// Ctrl+C handler
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			go func() {
				<-ctx.Done()
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			}()

			useJSON := jsonOutput || rawOutput

			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						return nil
					}
					if ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("read: %w", err)
				}

				if useJSON {
					fmt.Println(string(msg))
					continue
				}

				// Human-readable output
				var evt struct {
					Type        string          `json:"type"`
					ContainerID string          `json:"container_id"`
					Timestamp   time.Time       `json:"timestamp"`
					Data        json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(msg, &evt); err != nil {
					fmt.Println(string(msg))
					continue
				}

				ts := evt.Timestamp.Local().Format("15:04:05")
				fmt.Printf("[%s] %s  container=%s\n", ts, evt.Type, evt.ContainerID)

				switch evt.Type {
				case "usage_update":
					var d struct {
						Usage    map[string]int64 `json:"usage"`
						Limits   map[string]int64 `json:"limits"`
						Enforced map[string]bool  `json:"enforced"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						for _, lt := range []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending",
							"ram-usage-bsec", "disk-usage-bsec", "ram-request-bsec", "disk-request-bsec"} {
							u, uOk := d.Usage[lt]
							l, lOk := d.Limits[lt]
							if (uOk && u != 0) || (lOk && l != 0) {
								fmt.Printf("  %s: %s / %s", lt, formatValue(lt, u), formatValue(lt, l))
								if d.Enforced[lt] {
									fmt.Print(" ENFORCED")
								}
								fmt.Println()
							}
						}
					}
				case "limit_change":
					var d struct {
						LimitType string `json:"limit_type"`
						OldValue  int64  `json:"old_value"`
						NewValue  int64  `json:"new_value"`
						Operation string `json:"operation"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  %s: %s -> %s (%s)\n",
							d.LimitType,
							formatValue(d.LimitType, d.OldValue),
							formatValue(d.LimitType, d.NewValue),
							d.Operation)
					}
				case "enforcement_change":
					var d struct {
						LimitType string `json:"limit_type"`
						Enforced  bool   `json:"enforced"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						state := "RELEASED"
						if d.Enforced {
							state = "ENFORCED"
						}
						fmt.Printf("  %s: %s\n", d.LimitType, state)
					}
				case "container_register":
					var d struct {
						DockerID string `json:"docker_id"`
						Name     string `json:"name"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  name=%s docker_id=%s\n", d.Name, d.DockerID)
					}
				case "container_remove":
					fmt.Println("  removed")
				case "ollama_enqueue":
					var d struct {
						Model string `json:"model"`
						Bid   int64  `json:"bid"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  model=%s bid=%d milli-cents/wall-sec\n", d.Model, d.Bid)
					}
				case "ollama_dequeue":
					var d struct {
						Model       string  `json:"model"`
						WallSeconds float64 `json:"wall_seconds"`
						Cost        int64   `json:"cost"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  model=%s wall=%.1fs cost=%d milli-cents\n", d.Model, d.WallSeconds, d.Cost)
					}
				case "ollama_cancel":
					var d struct {
						Reason string `json:"reason"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  reason=%s\n", d.Reason)
					}
				case "ollama_bid_change":
					var d struct {
						Bid int64 `json:"bid"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  bid=%d milli-cents/wall-sec\n", d.Bid)
					}
				case "system_sleep":
					var d struct {
						DurationSeconds float64 `json:"duration_seconds"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  duration=%.1fs\n", d.DurationSeconds)
					}
				case "container_frozen":
					fmt.Println("  frozen (byte-sec accumulators suspended)")
				case "container_unfrozen":
					var d struct {
						EnforcementActive bool `json:"enforcement_active"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						if d.EnforcementActive {
							fmt.Println("  unfrozen (enforcement still active)")
						} else {
							fmt.Println("  unfrozen")
						}
					}
				case "container_killed":
					var d struct {
						LimitType   string `json:"limit_type"`
						UsageAtKill int64  `json:"usage_at_kill"`
						LimitAtKill int64  `json:"limit_at_kill"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  killed: %s (usage=%s, limit=%s)\n",
							d.LimitType, formatValue(d.LimitType, d.UsageAtKill), formatValue(d.LimitType, d.LimitAtKill))
					}
				case "proxy_upstream_error":
					var d struct {
						Host         string `json:"host"`
						StatusCode   int    `json:"status_code"`
						ErrorType    string `json:"error_type"`
						ErrorMessage string `json:"error_message"`
					}
					if json.Unmarshal(evt.Data, &d) == nil {
						fmt.Printf("  upstream error from %s (HTTP %d): %s — %s\n", d.Host, d.StatusCode, d.ErrorType, d.ErrorMessage)
					}
				}
				fmt.Println()
			}
		},
	}

	cmd.Flags().StringVar(&containerFilter, "container", "", "Filter by container ID or name (comma-separated for multiple)")
	cmd.Flags().StringVar(&typesFilter, "types", "", "Filter by event types (comma-separated)")
	cmd.Flags().BoolVar(&rawOutput, "raw", false, "Output one JSON object per line (NDJSON)")

	return cmd
}

func buildWSURL(containerFilter, typesFilter string) (string, error) {
	return buildWSURLFrom(apiURL, containerFilter, typesFilter), nil
}

func buildWSURLFrom(base, containerFilter, typesFilter string) string {
	wsBase := strings.Replace(base, "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)

	u, err := url.Parse(wsBase + "/events")
	if err != nil {
		return wsBase + "/events"
	}

	q := u.Query()
	if containerFilter != "" {
		q.Set("container_id", containerFilter)
	}
	if typesFilter != "" {
		q.Set("types", typesFilter)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

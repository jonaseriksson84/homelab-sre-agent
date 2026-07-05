// Package tools is the read-only tool registry: one implementation of each
// query tool, consumed by two frontends — the Claude tool-use loop during
// Escalation, and the MCP server. Every tool is a read (Docker via the
// GET-only proxy per ADR-0001) and every result is byte-capped.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
)

// resultByteCap bounds each tool result so one verbose query can't blow up
// the context window. Newest data is kept, like the log byte budget.
const resultByteCap = 16384

// paramLogMax bounds the params echoed into the audit log line.
const paramLogMax = 200

type Config struct {
	LokiURL            string
	LokiContainerLabel string
	PrometheusURL      string
	DockerProxyURL     string
}

type Registry struct {
	cfg    Config
	store  *store.Store
	client *http.Client
	log    *slog.Logger
}

func New(cfg Config, st *store.Store, log *slog.Logger) *Registry {
	return &Registry{cfg: cfg, store: st, client: &http.Client{Timeout: 30 * time.Second}, log: log}
}

// Tool describes one registry entry for a frontend: name, model-facing
// description, and a JSON schema for its parameters.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

func (r *Registry) Tools() []Tool {
	return []Tool{
		{
			Name: "query_loki",
			Description: "Query Loki for a container's log lines over an arbitrary window. " +
				"The Context Bundle already contains the alert target's logs ±15 minutes around " +
				"the incident — use this for OTHER containers or for wider/earlier windows.",
			InputSchema: objSchema(map[string]any{
				"container":     map[string]any{"type": "string", "description": "Container name whose logs to fetch"},
				"since_minutes": map[string]any{"type": "integer", "description": "Window start, minutes before now (default 30)"},
				"until_minutes": map[string]any{"type": "integer", "description": "Window end, minutes before now (default 0 = now)"},
			}, []string{"container"}),
		},
		{
			Name: "query_prometheus",
			Description: "Run an ad-hoc PromQL query. The Context Bundle already contains the " +
				"standard panel (node CPU/memory/disk/load and the target's container metrics, " +
				"last 30m) — use this for other metrics, other containers, or longer ranges.",
			InputSchema: objSchema(map[string]any{
				"query":         map[string]any{"type": "string", "description": "PromQL expression"},
				"range_minutes": map[string]any{"type": "integer", "description": "Range query over the last N minutes; 0 or omitted = instant query"},
			}, []string{"query"}),
		},
		{
			Name:        "list_containers",
			Description: "List all Docker containers with their states. The Context Bundle already contains this snapshot from incident time; use this for a fresh view.",
			InputSchema: objSchema(map[string]any{}, nil),
		},
		{
			Name:        "inspect_container",
			Description: "Inspect one Docker container: state, exit code, restart count, image, started-at.",
			InputSchema: objSchema(map[string]any{
				"name": map[string]any{"type": "string", "description": "Container name"},
			}, []string{"name"}),
		},
		{
			Name: "get_incidents",
			Description: "Query this agent's own incident history (beyond the Incident Memory one-liners already in the bundle): " +
				"prior incidents with their final diagnoses, filterable by target container and/or alertname.",
			InputSchema: objSchema(map[string]any{
				"target":    map[string]any{"type": "string", "description": "Filter: incidents for this target container"},
				"alertname": map[string]any{"type": "string", "description": "Filter: incidents that included this alertname"},
				"days":      map[string]any{"type": "integer", "description": "Lookback window in days (default 30)"},
				"limit":     map[string]any{"type": "integer", "description": "Max incidents returned (default 10)"},
			}, nil),
		},
	}
}

func objSchema(props map[string]any, required []string) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Execute runs one tool by name. Errors are returned for the frontend to
// render as a tool error (never fatal to the caller's flow); successful
// results are byte-capped with the newest data kept.
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	r.log.Info("tool call", "tool", name, "params", truncateForLog(string(input)))
	var out string
	var err error
	switch name {
	case "query_loki":
		out, err = r.queryLoki(ctx, input)
	case "query_prometheus":
		out, err = r.queryPrometheus(ctx, input)
	case "list_containers":
		out, err = r.listContainers(ctx)
	case "inspect_container":
		out, err = r.inspectContainer(ctx, input)
	case "get_incidents":
		out, err = r.getIncidents(input)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", err
	}
	return capResult(out), nil
}

// capResult enforces the per-result byte cap, keeping the newest (last) data.
func capResult(s string) string {
	if len(s) <= resultByteCap {
		return s
	}
	tail := s[len(s)-resultByteCap:]
	return fmt.Sprintf("(result truncated to %d bytes; newest data kept)\n%s", resultByteCap, tail)
}

func truncateForLog(s string) string {
	if len(s) <= paramLogMax {
		return s
	}
	return s[:paramLogMax] + "…"
}

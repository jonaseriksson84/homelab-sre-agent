// Package gather builds the Context Bundle: a deterministic, size-bounded
// text snapshot of Loki logs, Prometheus metrics, and Docker container states
// around an incident. Unavailable sources are noted in the bundle, never fatal.
package gather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	LokiURL            string
	LokiContainerLabel string
	PrometheusURL      string
	DockerProxyURL     string
	LogByteBudget      int
}

type Gatherer struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Gatherer {
	return &Gatherer{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

// Bundle is the rendered Context Bundle handed to the model.
type Bundle struct {
	Target string // empty means host-level only
	Text   string
}

// ResolveTarget maps alert labels to a container name: the `container` label
// first, then a fuzzy match of the alertname against running container names,
// else "" (host-level bundle).
func (g *Gatherer) ResolveTarget(ctx context.Context, labels map[string]string) string {
	if c := labels["container"]; c != "" {
		return c
	}
	names, err := g.containerNames(ctx)
	if err != nil {
		return ""
	}
	candidates := []string{strings.ToLower(labels["alertname"])}
	for _, v := range labels {
		candidates = append(candidates, strings.ToLower(v))
	}
	for _, name := range names {
		ln := strings.ToLower(name)
		for _, cand := range candidates {
			if cand != "" && (strings.Contains(cand, ln) || strings.Contains(ln, cand)) {
				return name
			}
		}
	}
	return ""
}

// Gather assembles the bundle for a target (or host-level if target is "")
// around the given incident time.
func (g *Gatherer) Gather(ctx context.Context, target string, at time.Time) Bundle {
	var b strings.Builder
	fmt.Fprintf(&b, "# Context Bundle\nTime: %s\nTarget: %s\n", at.UTC().Format(time.RFC3339), orHost(target))

	b.WriteString("\n## Docker container states\n")
	if states, err := g.dockerStates(ctx); err != nil {
		fmt.Fprintf(&b, "SOURCE UNAVAILABLE: docker proxy: %v\n", err)
	} else {
		b.WriteString(states)
	}

	b.WriteString("\n## Prometheus metrics (last 30m, downsampled)\n")
	if metrics, err := g.promPanel(ctx, target, at); err != nil {
		fmt.Fprintf(&b, "SOURCE UNAVAILABLE: prometheus: %v\n", err)
	} else {
		b.WriteString(metrics)
	}

	if target != "" {
		fmt.Fprintf(&b, "\n## Logs for %s (±15m window)\n", target)
		if logs, err := g.lokiLogs(ctx, target, at); err != nil {
			fmt.Fprintf(&b, "SOURCE UNAVAILABLE: loki: %v\n", err)
		} else {
			b.WriteString(logs)
		}
	}

	return Bundle{Target: target, Text: b.String()}
}

func orHost(target string) string {
	if target == "" {
		return "(none — host-level diagnosis)"
	}
	return target
}

// --- Docker ---

type dockerContainer struct {
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
	Image  string   `json:"Image"`
}

func (g *Gatherer) dockerContainers(ctx context.Context) ([]dockerContainer, error) {
	var cs []dockerContainer
	err := g.getJSON(ctx, g.cfg.DockerProxyURL+"/containers/json?all=true", &cs)
	return cs, err
}

func (g *Gatherer) containerNames(ctx context.Context) ([]string, error) {
	cs, err := g.dockerContainers(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range cs {
		if len(c.Names) > 0 {
			names = append(names, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	return names, nil
}

func (g *Gatherer) dockerStates(ctx context.Context) (string, error) {
	cs, err := g.dockerContainers(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		fmt.Fprintf(&b, "- %s: %s (%s) image=%s\n", name, c.State, c.Status, c.Image)
	}
	return b.String(), nil
}

// --- Prometheus ---

type promQuery struct {
	label string
	query string
}

func promPanelQueries(target string) []promQuery {
	qs := []promQuery{
		{"node CPU busy %", `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`},
		{"node memory used %", `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`},
		{"node disk used % (/)", `100 * (1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"})`},
		{"node load1", `node_load1`},
	}
	if target != "" {
		qs = append(qs,
			promQuery{"container CPU cores", fmt.Sprintf(`rate(container_cpu_usage_seconds_total{name="%s"}[5m])`, target)},
			promQuery{"container memory bytes", fmt.Sprintf(`container_memory_usage_bytes{name="%s"}`, target)},
			promQuery{"container restarts (30m)", fmt.Sprintf(`increase(container_restart_count{name="%s"}[30m])`, target)},
		)
	}
	return qs
}

type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Values [][2]any `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (g *Gatherer) promPanel(ctx context.Context, target string, at time.Time) (string, error) {
	start := at.Add(-30 * time.Minute)
	var b strings.Builder
	var firstErr error
	for _, q := range promPanelQueries(target) {
		u := fmt.Sprintf("%s/api/v1/query_range?%s", g.cfg.PrometheusURL, url.Values{
			"query": {q.query},
			"start": {strconv.FormatInt(start.Unix(), 10)},
			"end":   {strconv.FormatInt(at.Unix(), 10)},
			"step":  {"120"}, // ~15 points over 30m
		}.Encode())
		var resp promRangeResponse
		if err := g.getJSON(ctx, u, &resp); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(resp.Data.Result) == 0 {
			fmt.Fprintf(&b, "%s: no data\n", q.label)
			continue
		}
		var points []string
		for _, v := range resp.Data.Result[0].Values {
			if s, ok := v[1].(string); ok {
				points = append(points, compactNumber(s))
			}
		}
		fmt.Fprintf(&b, "%s: %s\n", q.label, strings.Join(points, " "))
	}
	if b.Len() == 0 && firstErr != nil {
		return "", firstErr
	}
	return b.String(), nil
}

func compactNumber(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', 4, 64)
}

// --- Loki ---

type lokiResponse struct {
	Data struct {
		Result []struct {
			Values [][2]string `json:"values"` // [ns-timestamp, line]
		} `json:"result"`
	} `json:"data"`
}

func (g *Gatherer) lokiLogs(ctx context.Context, target string, at time.Time) (string, error) {
	start := at.Add(-15 * time.Minute)
	end := at.Add(15 * time.Minute)
	u := fmt.Sprintf("%s/loki/api/v1/query_range?%s", g.cfg.LokiURL, url.Values{
		"query":     {fmt.Sprintf(`{%s=%q}`, g.cfg.LokiContainerLabel, target)},
		"start":     {strconv.FormatInt(start.UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {"1000"},
		"direction": {"backward"},
	}.Encode())
	var resp lokiResponse
	if err := g.getJSON(ctx, u, &resp); err != nil {
		return "", err
	}

	type entry struct {
		ts   int64
		line string
	}
	var entries []entry
	for _, stream := range resp.Data.Result {
		for _, v := range stream.Values {
			ts, _ := strconv.ParseInt(v[0], 10, 64)
			entries = append(entries, entry{ts, v[1]})
		}
	}
	if len(entries) == 0 {
		return "(no log lines in window)\n", nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ts < entries[j].ts })

	// Enforce the byte budget keeping the newest lines.
	total := 0
	keepFrom := len(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		lineLen := len(entries[i].line) + 1
		if total+lineLen > g.cfg.LogByteBudget {
			break
		}
		total += lineLen
		keepFrom = i
	}
	var b strings.Builder
	if keepFrom > 0 {
		fmt.Fprintf(&b, "(%d earlier lines truncated to fit byte budget)\n", keepFrom)
	}
	for _, e := range entries[keepFrom:] {
		b.WriteString(e.line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// --- shared ---

func (g *Gatherer) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

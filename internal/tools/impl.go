package tools

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

// --- query_loki ---

func (r *Registry) queryLoki(ctx context.Context, input json.RawMessage) (string, error) {
	var p struct {
		Container    string `json:"container"`
		SinceMinutes int    `json:"since_minutes"`
		UntilMinutes int    `json:"until_minutes"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("bad params: %w", err)
	}
	if p.Container == "" {
		return "", fmt.Errorf("container is required")
	}
	if p.SinceMinutes <= 0 {
		p.SinceMinutes = 30
	}
	now := time.Now()
	start := now.Add(-time.Duration(p.SinceMinutes) * time.Minute)
	end := now.Add(-time.Duration(p.UntilMinutes) * time.Minute)
	if !end.After(start) {
		return "", fmt.Errorf("empty window: since_minutes must exceed until_minutes")
	}

	u := fmt.Sprintf("%s/loki/api/v1/query_range?%s", r.cfg.LokiURL, url.Values{
		"query":     {fmt.Sprintf(`{%s=%q}`, r.cfg.LokiContainerLabel, p.Container)},
		"start":     {strconv.FormatInt(start.UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {"1000"},
		"direction": {"backward"},
	}.Encode())
	var resp struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, u, &resp); err != nil {
		return "", fmt.Errorf("loki: %w", err)
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
		return fmt.Sprintf("(no log lines for %s in window)\n", p.Container), nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ts < entries[j].ts })
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// --- query_prometheus ---

func (r *Registry) queryPrometheus(ctx context.Context, input json.RawMessage) (string, error) {
	var p struct {
		Query        string `json:"query"`
		RangeMinutes int    `json:"range_minutes"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("bad params: %w", err)
	}
	if p.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	now := time.Now()
	var u string
	if p.RangeMinutes > 0 {
		start := now.Add(-time.Duration(p.RangeMinutes) * time.Minute)
		step := max(15, p.RangeMinutes*60/20) // ~20 points regardless of range
		u = fmt.Sprintf("%s/api/v1/query_range?%s", r.cfg.PrometheusURL, url.Values{
			"query": {p.Query},
			"start": {strconv.FormatInt(start.Unix(), 10)},
			"end":   {strconv.FormatInt(now.Unix(), 10)},
			"step":  {strconv.Itoa(step)},
		}.Encode())
	} else {
		u = fmt.Sprintf("%s/api/v1/query?%s", r.cfg.PrometheusURL, url.Values{
			"query": {p.Query},
		}.Encode())
	}

	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`  // instant
				Values [][2]any          `json:"values"` // range
			} `json:"result"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, u, &resp); err != nil {
		return "", fmt.Errorf("prometheus: %w", err)
	}
	if resp.Status != "success" {
		return "", fmt.Errorf("prometheus: %s", resp.Error)
	}
	if len(resp.Data.Result) == 0 {
		return "(no data)\n", nil
	}

	var b strings.Builder
	for _, series := range resp.Data.Result {
		var points []string
		if len(series.Values) > 0 {
			for _, v := range series.Values {
				if s, ok := v[1].(string); ok {
					points = append(points, compactNumber(s))
				}
			}
		} else if s, ok := series.Value[1].(string); ok {
			points = append(points, compactNumber(s))
		}
		fmt.Fprintf(&b, "%s: %s\n", labelsOf(series.Metric), strings.Join(points, " "))
	}
	return b.String(), nil
}

func labelsOf(metric map[string]string) string {
	if len(metric) == 0 {
		return "(series)"
	}
	keys := make([]string, 0, len(metric))
	for k := range metric {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, metric[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func compactNumber(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', 4, 64)
}

// --- list_containers / inspect_container ---

func (r *Registry) listContainers(ctx context.Context) (string, error) {
	var cs []struct {
		Names  []string `json:"Names"`
		State  string   `json:"State"`
		Status string   `json:"Status"`
		Image  string   `json:"Image"`
	}
	if err := r.getJSON(ctx, r.cfg.DockerProxyURL+"/containers/json?all=true", &cs); err != nil {
		return "", fmt.Errorf("docker proxy: %w", err)
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

func (r *Registry) inspectContainer(ctx context.Context, input json.RawMessage) (string, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("bad params: %w", err)
	}
	if p.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	var c struct {
		Name  string `json:"Name"`
		State struct {
			Status     string `json:"Status"`
			ExitCode   int    `json:"ExitCode"`
			OOMKilled  bool   `json:"OOMKilled"`
			Restarting bool   `json:"Restarting"`
			StartedAt  string `json:"StartedAt"`
			FinishedAt string `json:"FinishedAt"`
		} `json:"State"`
		RestartCount int `json:"RestartCount"`
		Config       struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	u := r.cfg.DockerProxyURL + "/containers/" + url.PathEscape(p.Name) + "/json"
	if err := r.getJSON(ctx, u, &c); err != nil {
		return "", fmt.Errorf("docker proxy: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\nstatus: %s\nimage: %s\nrestart_count: %d\nexit_code: %d\noom_killed: %v\nstarted_at: %s\n",
		strings.TrimPrefix(c.Name, "/"), c.State.Status, c.Config.Image, c.RestartCount,
		c.State.ExitCode, c.State.OOMKilled, c.State.StartedAt)
	if c.State.FinishedAt != "" && !strings.HasPrefix(c.State.FinishedAt, "0001-") {
		fmt.Fprintf(&b, "finished_at: %s\n", c.State.FinishedAt)
	}
	return b.String(), nil
}

// --- get_incidents ---

func (r *Registry) getIncidents(input json.RawMessage) (string, error) {
	var p struct {
		Target    string `json:"target"`
		Alertname string `json:"alertname"`
		Days      int    `json:"days"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("bad params: %w", err)
	}
	if p.Days <= 0 {
		p.Days = 30
	}
	if p.Limit <= 0 {
		p.Limit = 10
	}
	since := time.Now().AddDate(0, 0, -p.Days)
	incs, err := r.store.FindIncidents(p.Target, p.Alertname, since, p.Limit)
	if err != nil {
		return "", fmt.Errorf("incident store: %w", err)
	}
	if len(incs) == 0 {
		return fmt.Sprintf("(no incidents match in the last %d days)\n", p.Days), nil
	}
	var b strings.Builder
	for _, inc := range incs {
		what := inc.AlertNames
		if what == "" {
			what = inc.Target
		}
		verdict := inc.EscalationOutput
		if verdict == "" {
			var tr struct {
				LikelyCause string `json:"likely_cause"`
			}
			if json.Unmarshal([]byte(inc.TriageOutput), &tr) == nil && tr.LikelyCause != "" {
				verdict = tr.LikelyCause
			}
		}
		if verdict == "" {
			verdict = "no diagnosis recorded"
		}
		verdict = strings.Join(strings.Fields(verdict), " ")
		if rs := []rune(verdict); len(rs) > 300 {
			verdict = string(rs[:300]) + "…"
		}
		outcome := "STILL OPEN"
		if inc.ResolvedAt != nil {
			outcome = "resolved after " + inc.ResolvedAt.Sub(inc.CreatedAt).Round(time.Minute).String()
		}
		fmt.Fprintf(&b, "- #%d %s (%s) target=%s %s: %s — %s\n",
			inc.ID, inc.CreatedAt.Format(time.RFC3339), inc.Source, inc.Target, what, verdict, outcome)
	}
	return b.String(), nil
}

// --- shared ---

func (r *Registry) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

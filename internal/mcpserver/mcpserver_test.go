package mcpserver_test

// Tests act as an MCP client speaking raw JSON-RPC over the streamable HTTP
// endpoint (initialize → tools/list → tools/call) against the real handler,
// with Loki/Prometheus/docker fakes and a real temp-file SQLite store behind
// the registry. The keystone scenario creates an incident through the webhook
// pipeline and reads it back over MCP: one implementation, two frontends.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/gather"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/mcpserver"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/notify"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/pipeline"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/tools"
)

// --- backend fakes ---

type fakes struct {
	loki, prom, docker, anthropic, ntfy *httptest.Server

	mu          sync.Mutex
	lokiQueries []string
	lokiLines   []string
	lokiDown    bool
}

func newFakes(t *testing.T) *fakes {
	f := &fakes{lokiLines: []string{"2026/07/06 nginx: [emerg] host not found in upstream"}}

	f.loki = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lokiQueries = append(f.lokiQueries, r.URL.Query().Get("query"))
		if f.lokiDown {
			http.Error(w, "loki is down", http.StatusInternalServerError)
			return
		}
		values := make([][2]string, len(f.lokiLines))
		for i, line := range f.lokiLines {
			values[i] = [2]string{fmt.Sprintf("%d", time.Now().UnixNano()-int64(len(f.lokiLines)-i)*1e9), line}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"result": []map[string]any{{"values": values}}},
		})
	}))

	f.prom = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"result": []map[string]any{
				{"metric": map[string]string{"name": "nginx"}, "values": [][2]any{{1e9, "42.5"}}},
			}},
		})
	}))

	f.docker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"Names": []string{"/nginx"}, "State": "restarting", "Status": "Restarting (1) 5s ago", "Image": "nginx:latest"},
		})
	}))

	f.anthropic = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr := claude.Triage{
			Summary: "nginx is crash-looping.", LikelyCause: "bad config after last edit",
			Severity: "warning", Confidence: 0.9,
		}
		b, _ := json.Marshal(tr)
		json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": string(b)}},
			"stop_reason": "end_turn",
		})
	}))

	f.ntfy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	t.Cleanup(func() {
		for _, s := range []*httptest.Server{f.loki, f.prom, f.docker, f.anthropic, f.ntfy} {
			s.Close()
		}
	})
	return f
}

// setup builds the real MCP handler over a registry backed by the fakes and a
// real store, plus the webhook pipeline sharing that store.
func setup(t *testing.T, f *fakes) (*mcpClient, *pipeline.Pipeline) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := tools.New(tools.Config{
		LokiURL:            f.loki.URL,
		LokiContainerLabel: "container",
		PrometheusURL:      f.prom.URL,
		DockerProxyURL:     f.docker.URL,
	}, st, log)

	srv := httptest.NewServer(mcpserver.Handler(reg, "test", log))
	t.Cleanup(srv.Close)

	p := &pipeline.Pipeline{
		Gatherer: gather.New(gather.Config{
			LokiURL:            f.loki.URL,
			LokiContainerLabel: "container",
			PrometheusURL:      f.prom.URL,
			DockerProxyURL:     f.docker.URL,
			LogByteBudget:      2048,
		}),
		Claude: claude.New(claude.Config{
			BaseURL: f.anthropic.URL, APIKey: "test-key",
			TriageModel: "test-haiku", EscalationModel: "test-opus",
		}),
		Store:               st,
		Notifier:            notify.NewNtfy(f.ntfy.URL, "test-topic"),
		ConfidenceThreshold: 0.7,
		TriageModel:         "test-haiku",
		EscalationModel:     "test-opus",
		Log:                 log,
	}
	return newMCPClient(t, srv.URL), p
}

// --- raw JSON-RPC streamable HTTP client ---

type mcpClient struct {
	t       *testing.T
	url     string
	session string
	nextID  int
}

func newMCPClient(t *testing.T, url string) *mcpClient {
	return &mcpClient{t: t, url: url, nextID: 1}
}

// post sends one JSON-RPC message; a nil id means notification (no response).
func (c *mcpClient) post(body map[string]any) (map[string]any, *http.Response) {
	c.t.Helper()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if resp.StatusCode == http.StatusAccepted { // notification
		return nil, resp
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("MCP endpoint status %d: %s", resp.StatusCode, b)
	}

	var raw []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "data: ") {
				raw = []byte(strings.TrimPrefix(line, "data: "))
				break
			}
		}
	} else {
		raw, _ = io.ReadAll(resp.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("bad JSON-RPC response %q: %v", raw, err)
	}
	return out, resp
}

func (c *mcpClient) call(method string, params map[string]any) map[string]any {
	c.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	id := c.nextID
	c.nextID++
	msg, _ := c.post(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if msg == nil {
		c.t.Fatalf("no response for %s", method)
	}
	if rpcErr, ok := msg["error"]; ok {
		c.t.Fatalf("%s returned protocol error: %v", method, rpcErr)
	}
	result, _ := msg["result"].(map[string]any)
	return result
}

// initialize performs the MCP handshake and returns the initialize result.
func (c *mcpClient) initialize() map[string]any {
	c.t.Helper()
	result := c.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "0"},
	})
	c.post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}})
	return result
}

// callTool runs tools/call and returns (concatenated text, isError).
func (c *mcpClient) callTool(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	result := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	var text strings.Builder
	blocks, _ := result["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["type"] == "text" {
			text.WriteString(block["text"].(string))
		}
	}
	return text.String(), result["isError"] == true
}

// --- scenarios ---

func TestInitializeAdvertisesTools(t *testing.T) {
	c, _ := setup(t, newFakes(t))
	result := c.initialize()

	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "sre-agent" {
		t.Errorf("serverInfo.name = %v, want sre-agent", info["name"])
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities missing tools: %v", caps)
	}
}

func TestToolsListNamesExactlyTheRegistryTools(t *testing.T) {
	c, _ := setup(t, newFakes(t))
	c.initialize()

	result := c.call("tools/list", nil)
	list, _ := result["tools"].([]any)
	got := map[string]bool{}
	for _, item := range list {
		tool, _ := item.(map[string]any)
		got[tool["name"].(string)] = true
	}
	want := []string{"query_loki", "query_prometheus", "list_containers", "inspect_container", "get_incidents"}
	if len(got) != len(want) {
		t.Errorf("tools/list has %d tools, want %d: %v", len(got), len(want), got)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tools/list missing %s", name)
		}
	}
}

func TestIncidentCreatedViaWebhookIsReadableOverMCP(t *testing.T) {
	f := newFakes(t)
	c, p := setup(t, f)
	ctx := context.Background()

	// Create and resolve an incident through the webhook pipeline.
	wh := pipeline.Webhook{
		GroupKey: "group1", Status: "firing",
		Labels: map[string]string{"alertname": "ContainerRestarting", "container": "nginx"},
		Alerts: []string{"ContainerRestarting"},
	}
	if err := p.HandleWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // RFC3339 second resolution
	if err := p.HandleWebhook(ctx, pipeline.Webhook{GroupKey: "group1", Status: "resolved"}); err != nil {
		t.Fatal(err)
	}

	c.initialize()
	text, isError := c.callTool("get_incidents", map[string]any{"target": "nginx"})
	if isError {
		t.Fatalf("get_incidents errored: %s", text)
	}
	if !strings.Contains(text, "bad config after last edit") {
		t.Errorf("incident diagnosis not visible over MCP: %q", text)
	}
	if !strings.Contains(text, "resolved after") {
		t.Errorf("incident outcome not visible over MCP: %q", text)
	}
}

func TestQueryLokiHitsBackendWithRequestedSelector(t *testing.T) {
	f := newFakes(t)
	c, _ := setup(t, f)
	c.initialize()

	text, isError := c.callTool("query_loki", map[string]any{"container": "postgres", "since_minutes": 60})
	if isError {
		t.Fatalf("query_loki errored: %s", text)
	}
	if !strings.Contains(text, "host not found in upstream") {
		t.Errorf("log lines missing from result: %q", text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lokiQueries) != 1 || !strings.Contains(f.lokiQueries[0], `container="postgres"`) {
		t.Errorf("loki received %v, want a postgres selector", f.lokiQueries)
	}
}

func TestBackendFailureIsToolErrorNotProtocolError(t *testing.T) {
	f := newFakes(t)
	c, _ := setup(t, f)
	f.mu.Lock()
	f.lokiDown = true
	f.mu.Unlock()
	c.initialize()

	text, isError := c.callTool("query_loki", map[string]any{"container": "nginx"})
	if !isError {
		t.Errorf("loki 500 did not produce an error tool result: %q", text)
	}
	if !strings.Contains(text, "loki") {
		t.Errorf("error result does not name the failing source: %q", text)
	}
}

func TestOversizedResultCarriesTruncationNote(t *testing.T) {
	f := newFakes(t)
	c, _ := setup(t, f)
	f.mu.Lock()
	f.lokiLines = nil
	for i := 0; i < 400; i++ {
		f.lokiLines = append(f.lokiLines, fmt.Sprintf("line-%03d some verbose log output padding padding padding padding padding padding", i))
	}
	f.mu.Unlock()
	c.initialize()

	text, isError := c.callTool("query_loki", map[string]any{"container": "nginx"})
	if isError {
		t.Fatalf("query_loki errored: %s", text)
	}
	if !strings.Contains(text, "truncated") {
		t.Error("oversized result missing truncation note")
	}
	if len(text) > 17000 {
		t.Errorf("result %d bytes, exceeds the byte cap", len(text))
	}
	if !strings.Contains(text, "line-399") {
		t.Error("newest line missing — truncation kept old data")
	}
}

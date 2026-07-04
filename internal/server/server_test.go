package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/gather"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/notify"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/pipeline"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/server"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
)

// jsonHandler returns a fake dependency that always responds with body.
func jsonHandler(body any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(body)
	}))
}

func newTestServer(t *testing.T) (*server.Server, *store.Store, *httptest.Server) {
	t.Helper()
	empty := jsonHandler(map[string]any{"data": map[string]any{"result": []any{}}})
	docker := jsonHandler([]any{})
	triageText, _ := json.Marshal(claude.Triage{Summary: "ok", LikelyCause: "x", Severity: "info", Confidence: 0.95})
	anthropic := jsonHandler(map[string]any{
		"content":     []map[string]any{{"type": "text", "text": string(triageText)}},
		"stop_reason": "end_turn",
	})
	ntfy := jsonHandler(map[string]any{})
	t.Cleanup(func() { empty.Close(); docker.Close(); anthropic.Close(); ntfy.Close() })

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	p := &pipeline.Pipeline{
		Gatherer: gather.New(gather.Config{
			LokiURL: empty.URL, LokiContainerLabel: "container",
			PrometheusURL: empty.URL, DockerProxyURL: docker.URL, LogByteBudget: 2048,
		}),
		Claude:              claude.New(claude.Config{BaseURL: anthropic.URL, APIKey: "k", TriageModel: "h", EscalationModel: "o"}),
		Store:               st,
		Notifier:            notify.NewNtfy(ntfy.URL, "topic"),
		ConfidenceThreshold: 0.7,
		TriageModel:         "h",
		EscalationModel:     "o",
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := &server.Server{Pipeline: p, Log: p.Log}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, st, ts
}

func TestWebhookRespondsImmediatelyAndProcessesAsync(t *testing.T) {
	srv, st, ts := newTestServer(t)

	payload := `{
		"version": "4",
		"groupKey": "{}:{alertname=\"InstanceDown\"}",
		"status": "firing",
		"alerts": [{"labels": {"alertname": "InstanceDown"}}],
		"commonLabels": {"alertname": "InstanceDown"}
	}`
	resp, err := http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}

	srv.Wait() // let async processing finish
	inc, err := st.FindOpenByGroupKey(`{}:{alertname="InstanceDown"}`)
	if err != nil {
		t.Fatal(err)
	}
	if inc == nil {
		t.Fatal("webhook did not create an incident")
	}
	if inc.AlertNames != "InstanceDown" {
		t.Errorf("alert names = %q", inc.AlertNames)
	}
}

func TestWebhookRejectsInvalidPayload(t *testing.T) {
	_, _, ts := newTestServer(t)

	for name, body := range map[string]string{
		"not json":  "not json at all",
		"empty":     "{}",
		"no status": `{"groupKey": "g"}`,
	} {
		resp, err := http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

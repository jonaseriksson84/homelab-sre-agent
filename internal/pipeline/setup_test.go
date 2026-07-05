package pipeline_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/gather"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/notify"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/pipeline"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
)

func newPipeline(t *testing.T, f *fakes) (*pipeline.Pipeline, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	p := &pipeline.Pipeline{
		Gatherer: gather.New(gather.Config{
			LokiURL:            f.loki.URL,
			LokiContainerLabel: "container",
			PrometheusURL:      f.prom.URL,
			DockerProxyURL:     f.docker.URL,
			LogByteBudget:      2048,
		}),
		Claude: claude.New(claude.Config{
			BaseURL:         f.anthropic.URL,
			APIKey:          "test-key",
			TriageModel:     "test-haiku",
			EscalationModel: "test-opus",
		}),
		Store:               st,
		Notifier:            notify.NewNtfy(f.ntfy.URL, "test-topic"),
		ConfidenceThreshold: 0.7,
		TriageModel:         "test-haiku",
		EscalationModel:     "test-opus",
		MemoryWindowDays:    30,
		MemoryMaxEntries:    5,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return p, st
}

func webhookFiring(groupKey, container string) pipeline.Webhook {
	return pipeline.Webhook{
		GroupKey: groupKey,
		Status:   "firing",
		Labels:   map[string]string{"alertname": "ContainerRestarting", "container": container},
		Alerts:   []string{"ContainerRestarting"},
	}
}

func webhookResolved(groupKey string) pipeline.Webhook {
	return pipeline.Webhook{GroupKey: groupKey, Status: "resolved"}
}

func pipeline_Webhook(groupKey string, labels map[string]string) pipeline.Webhook {
	return pipeline.Webhook{
		GroupKey: groupKey,
		Status:   "firing",
		Labels:   labels,
		Alerts:   []string{labels["alertname"]},
	}
}

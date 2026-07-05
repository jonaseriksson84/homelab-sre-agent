// sre-agent diagnoses homelab incidents: `diagnose <container>` on demand,
// `serve` for Alertmanager webhooks + ntfy notifications.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/config"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/gather"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/notify"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/pipeline"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/server"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: sre-agent <diagnose <container> | serve>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	p := &pipeline.Pipeline{
		Gatherer: gather.New(gather.Config{
			LokiURL:            cfg.LokiURL,
			LokiContainerLabel: cfg.LokiContainerLabel,
			PrometheusURL:      cfg.PrometheusURL,
			DockerProxyURL:     cfg.DockerProxyURL,
			LogByteBudget:      cfg.LogByteBudget,
		}),
		Claude: claude.New(claude.Config{
			BaseURL:         cfg.AnthropicURL,
			APIKey:          cfg.AnthropicKey,
			TriageModel:     cfg.TriageModel,
			EscalationModel: cfg.EscalationModel,
		}),
		Store:    st,
		Notifier: notify.NewNtfy(cfg.NtfyURL, cfg.NtfyTopic),
		Tools: tools.New(tools.Config{
			LokiURL:            cfg.LokiURL,
			LokiContainerLabel: cfg.LokiContainerLabel,
			PrometheusURL:      cfg.PrometheusURL,
			DockerProxyURL:     cfg.DockerProxyURL,
		}, st, log),
		ToolBudget:          cfg.ToolBudget,
		ConfidenceThreshold: cfg.ConfidenceThreshold,
		TriageModel:         cfg.TriageModel,
		EscalationModel:     cfg.EscalationModel,
		MemoryWindowDays:    cfg.MemoryWindowDays,
		MemoryMaxEntries:    cfg.MemoryMaxEntries,
		Log:                 log,
	}

	switch os.Args[1] {
	case "diagnose":
		if len(os.Args) < 3 {
			return fmt.Errorf("usage: sre-agent diagnose <container>")
		}
		d, err := p.DiagnoseManual(context.Background(), os.Args[2])
		if err != nil {
			return err
		}
		fmt.Println(d.Text())
		fmt.Printf("\n(severity: %s, confidence: %.2f, model: %s)\n",
			d.Triage.Severity, d.Triage.Confidence, d.ModelUsed)
		return nil

	case "serve":
		srv := &server.Server{Pipeline: p, Log: log}
		log.Info("listening", "addr", cfg.ListenAddr)
		return http.ListenAndServe(cfg.ListenAddr, srv.Handler())

	default:
		return fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
}

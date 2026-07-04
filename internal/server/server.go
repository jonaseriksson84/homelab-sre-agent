// Package server exposes the Alertmanager webhook endpoint. The handler
// validates and enqueues, returning 2xx immediately; diagnosis runs
// asynchronously so Alertmanager never times out.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/pipeline"
)

// amPayload is the Alertmanager webhook JSON (version "4").
type amPayload struct {
	Version  string `json:"version"`
	GroupKey string `json:"groupKey"`
	Status   string `json:"status"`
	Alerts   []struct {
		Labels map[string]string `json:"labels"`
	} `json:"alerts"`
	CommonLabels map[string]string `json:"commonLabels"`
}

type Server struct {
	Pipeline *pipeline.Pipeline
	Log      *slog.Logger

	wg sync.WaitGroup
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload amPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.GroupKey == "" || payload.Status == "" {
		http.Error(w, "missing groupKey or status", http.StatusBadRequest)
		return
	}

	wh := pipeline.Webhook{
		GroupKey: payload.GroupKey,
		Status:   payload.Status,
		Labels:   payload.CommonLabels,
	}
	seen := map[string]bool{}
	for _, a := range payload.Alerts {
		if name := a.Labels["alertname"]; name != "" && !seen[name] {
			seen[name] = true
			wh.Alerts = append(wh.Alerts, name)
		}
		// Prefer per-alert labels for targeting when common labels lack them.
		if wh.Labels["container"] == "" && a.Labels["container"] != "" {
			if wh.Labels == nil {
				wh.Labels = map[string]string{}
			}
			wh.Labels["container"] = a.Labels["container"]
		}
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.Pipeline.HandleWebhook(ctx, wh); err != nil {
			s.Log.Error("webhook processing failed", "group_key", wh.GroupKey, "error", err)
		}
	}()

	w.WriteHeader(http.StatusAccepted)
}

// Wait blocks until in-flight webhook processing finishes (shutdown/tests).
func (s *Server) Wait() { s.wg.Wait() }

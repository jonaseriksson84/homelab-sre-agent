// Package pipeline orchestrates gather → triage → escalate → store → notify.
// Both entry points (CLI diagnose, webhook serve) drive this one pipeline.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/gather"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/notify"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
)

type Pipeline struct {
	Gatherer            *gather.Gatherer
	Claude              *claude.Client
	Store               *store.Store
	Notifier            notify.Notifier
	ConfidenceThreshold float64
	TriageModel         string
	EscalationModel     string
	Log                 *slog.Logger
}

// Diagnosis is the final answer for an Incident: the triage result, plus the
// escalated prose when escalation ran.
type Diagnosis struct {
	Triage    claude.Triage
	Escalated string // empty when escalation did not run
	ModelUsed string
}

// Text returns the operator-facing plain-language diagnosis.
func (d Diagnosis) Text() string {
	if d.Escalated != "" {
		return d.Escalated
	}
	return fmt.Sprintf("%s\n\nLikely cause: %s", d.Triage.Summary, d.Triage.LikelyCause)
}

// diagnose runs gather → triage → maybe escalate for a target.
func (p *Pipeline) diagnose(ctx context.Context, target string, at time.Time) (Diagnosis, gather.Bundle, error) {
	bundle := p.Gatherer.Gather(ctx, target, at)
	p.Log.Info("context bundle gathered", "target", target, "bytes", len(bundle.Text))

	triage, err := p.Claude.Triage(ctx, bundle.Text)
	if err != nil {
		return Diagnosis{}, bundle, fmt.Errorf("triage: %w", err)
	}
	d := Diagnosis{Triage: triage, ModelUsed: p.TriageModel}
	p.Log.Info("triage complete", "confidence", triage.Confidence,
		"severity", triage.Severity, "insufficient_evidence", triage.InsufficientEvidence)

	if triage.Confidence < p.ConfidenceThreshold || triage.InsufficientEvidence {
		p.Log.Info("escalating", "model", p.EscalationModel)
		escalated, err := p.Claude.Escalate(ctx, bundle.Text)
		if err != nil {
			// Conservative: a failed escalation still leaves us with the
			// triage diagnosis rather than nothing.
			p.Log.Error("escalation failed, keeping triage diagnosis", "error", err)
		} else {
			d.Escalated = escalated
			d.ModelUsed = p.EscalationModel
		}
	}
	return d, bundle, nil
}

// DiagnoseManual runs the pipeline for a CLI invocation: identical to the
// webhook path but stored with source `manual` and never notified.
func (p *Pipeline) DiagnoseManual(ctx context.Context, target string) (Diagnosis, error) {
	now := time.Now()
	d, _, err := p.diagnose(ctx, target, now)
	if err != nil {
		return d, err
	}
	inc := &store.Incident{
		Source:    "manual",
		Target:    target,
		Status:    "resolved", // a manual run is a one-shot episode
		CreatedAt: now,
		LastSeen:  now,
	}
	if err := p.Store.Create(inc); err != nil {
		return d, fmt.Errorf("storing incident: %w", err)
	}
	if err := p.recordDiagnosis(inc.ID, d, false); err != nil {
		return d, err
	}
	if err := p.Store.Resolve(inc.ID, now); err != nil {
		return d, err
	}
	return d, nil
}

// Webhook is the subset of the Alertmanager v4 payload the pipeline needs.
type Webhook struct {
	GroupKey string
	Status   string // firing | resolved
	Labels   map[string]string
	Alerts   []string // alertnames
}

// HandleWebhook applies the Incident lifecycle: first firing creates and
// diagnoses; repeat firing bumps last_seen; resolved closes with a
// low-priority ping. Flaps become new Incidents.
func (p *Pipeline) HandleWebhook(ctx context.Context, wh Webhook) error {
	now := time.Now()
	open, err := p.Store.FindOpenByGroupKey(wh.GroupKey)
	if err != nil {
		return err
	}

	switch wh.Status {
	case "resolved":
		if open == nil {
			p.Log.Info("resolved webhook for unknown group, ignoring", "group_key", wh.GroupKey)
			return nil
		}
		if err := p.Store.Resolve(open.ID, now); err != nil {
			return err
		}
		dur := now.Sub(open.CreatedAt).Round(time.Minute)
		msg := fmt.Sprintf("%s resolved after %s", open.AlertNames, dur)
		if err := p.Notifier.Notify(ctx, "Resolved: "+open.AlertNames, msg, notify.ResolvedPriority); err != nil {
			p.Log.Error("resolve notification failed", "error", err)
		}
		p.Log.Info("incident resolved", "id", open.ID, "duration", dur)
		return nil

	case "firing":
		if open != nil {
			p.Log.Info("repeat firing, bumping last_seen", "id", open.ID)
			return p.Store.TouchLastSeen(open.ID, now)
		}
		target := p.Gatherer.ResolveTarget(ctx, wh.Labels)
		inc := &store.Incident{
			Source:     "alertmanager",
			GroupKey:   wh.GroupKey,
			AlertNames: strings.Join(wh.Alerts, ","),
			Target:     target,
			Status:     "open",
			CreatedAt:  now,
			LastSeen:   now,
		}
		if err := p.Store.Create(inc); err != nil {
			return err
		}
		p.Log.Info("incident created", "id", inc.ID, "group_key", wh.GroupKey, "target", target)

		d, _, err := p.diagnose(ctx, target, now)
		if err != nil {
			p.Log.Error("diagnosis failed", "id", inc.ID, "error", err)
			return err
		}
		title := fmt.Sprintf("[%s] %s", d.Triage.Severity, inc.AlertNames)
		notifyErr := p.Notifier.Notify(ctx, title, d.Text(), notify.SeverityPriority(d.Triage.Severity))
		if notifyErr != nil {
			p.Log.Error("diagnosis notification failed", "error", notifyErr)
		}
		return p.recordDiagnosis(inc.ID, d, notifyErr == nil)

	default:
		return fmt.Errorf("unknown webhook status %q", wh.Status)
	}
}

func (p *Pipeline) recordDiagnosis(id int64, d Diagnosis, notified bool) error {
	triageJSON, err := json.Marshal(d.Triage)
	if err != nil {
		return err
	}
	return p.Store.SetDiagnosis(id, string(triageJSON), d.Triage.Confidence, d.Escalated, d.ModelUsed, notified)
}

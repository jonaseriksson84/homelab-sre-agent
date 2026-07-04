// Package claude calls the Anthropic Messages API for Triage (structured,
// cheap model) and Escalation (stronger model, same Context Bundle).
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Config struct {
	BaseURL         string
	APIKey          string
	TriageModel     string
	EscalationModel string
}

type Client struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, client: &http.Client{Timeout: 5 * time.Minute}}
}

// Triage is the structured result of the cheap first-pass diagnosis.
type Triage struct {
	Summary              string  `json:"summary"`
	LikelyCause          string  `json:"likely_cause"`
	Severity             string  `json:"severity"` // critical | warning | info
	Confidence           float64 `json:"confidence"`
	InsufficientEvidence bool    `json:"insufficient_evidence"`
}

var triageSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary":               map[string]any{"type": "string", "description": "One-paragraph plain-language diagnosis"},
		"likely_cause":          map[string]any{"type": "string", "description": "Most likely root cause, one sentence"},
		"severity":              map[string]any{"type": "string", "enum": []string{"critical", "warning", "info"}},
		"confidence":            map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		"insufficient_evidence": map[string]any{"type": "boolean"},
	},
	"required":             []string{"summary", "likely_cause", "severity", "confidence", "insufficient_evidence"},
	"additionalProperties": false,
}

const triagePrompt = `You are an SRE diagnosing a homelab incident. Below is a Context Bundle:
logs, metrics, and container states gathered around the incident. Diagnose what
most likely went wrong, in plain language a tired operator can act on. If the
evidence is insufficient for a diagnosis, say so via insufficient_evidence.

%s`

func (c *Client) Triage(ctx context.Context, bundle string) (Triage, error) {
	body := map[string]any{
		"model":      c.cfg.TriageModel,
		"max_tokens": 1024,
		"messages": []map[string]any{
			{"role": "user", "content": fmt.Sprintf(triagePrompt, bundle)},
		},
		"output_config": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"schema": triageSchema,
			},
		},
	}
	text, err := c.messages(ctx, body)
	if err != nil {
		return Triage{}, err
	}
	var t Triage
	if err := json.Unmarshal([]byte(text), &t); err != nil {
		return Triage{}, fmt.Errorf("parsing triage output: %w", err)
	}
	return t, nil
}

const escalationPrompt = `You are a senior SRE. A first-pass triage of this homelab incident had low
confidence. Re-diagnose from scratch using the Context Bundle below. Give a
plain-language diagnosis: what most likely went wrong, the evidence for it, and
what the operator should check or do first. Be concrete and brief.

%s`

// Escalate re-runs the diagnosis on the stronger model, returning prose.
func (c *Client) Escalate(ctx context.Context, bundle string) (string, error) {
	body := map[string]any{
		"model":      c.cfg.EscalationModel,
		"max_tokens": 2048,
		"thinking":   map[string]any{"type": "adaptive"},
		"messages": []map[string]any{
			{"role": "user", "content": fmt.Sprintf(escalationPrompt, bundle)},
		},
	}
	return c.messages(ctx, body)
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func (c *Client) messages(ctx context.Context, body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("anthropic API status %d: %s", resp.StatusCode, b)
	}
	var mr messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return "", err
	}
	for _, block := range mr.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text content in response (stop_reason=%s)", mr.StopReason)
}

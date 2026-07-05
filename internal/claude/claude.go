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
		"summary":      map[string]any{"type": "string", "description": "One-paragraph plain-language diagnosis"},
		"likely_cause": map[string]any{"type": "string", "description": "Most likely root cause, one sentence"},
		"severity":     map[string]any{"type": "string", "enum": []string{"critical", "warning", "info"}},
		// The structured-output validator rejects minimum/maximum on numbers;
		// the range lives in the description and is clamped after parsing.
		"confidence":            map[string]any{"type": "number", "description": "Confidence in the diagnosis, between 0 and 1"},
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
	t.Confidence = min(max(t.Confidence, 0), 1)
	return t, nil
}

const escalationPrompt = `You are a senior SRE. A first-pass triage of this homelab incident had low
confidence. Re-diagnose from scratch using the Context Bundle below. Give a
plain-language diagnosis: what most likely went wrong, the evidence for it, and
what the operator should check or do first. Be concrete and brief.

%s`

const escalationToolNote = `

You may use the provided read-only tools to gather evidence beyond the bundle
(other containers' logs, ad-hoc metrics, container details, incident history).
The tool budget is small — spend calls on NEW evidence, not on re-fetching
what the bundle already contains. Conclude with your final diagnosis as text.`

// Tool declares one tool for the escalation loop; the pipeline maps registry
// entries to this shape.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolFunc executes one requested tool call. An error becomes an error
// tool-result the model sees — never fatal to the loop.
type ToolFunc func(ctx context.Context, name string, input json.RawMessage) (string, error)

// Escalate re-runs the diagnosis on the stronger model, returning prose plus
// the number of tool calls executed. With no tools (or budget <= 0) it is a
// single-shot call, phase-3 behavior. Otherwise it runs a bounded tool loop:
// the model may call tools until the budget is spent, after which one final
// request with tool use disabled forces a conclusion.
func (c *Client) Escalate(ctx context.Context, bundle string, tools []Tool, exec ToolFunc, budget int) (string, int, error) {
	if len(tools) == 0 || exec == nil || budget <= 0 {
		body := map[string]any{
			"model":      c.cfg.EscalationModel,
			"max_tokens": 2048,
			"thinking":   map[string]any{"type": "adaptive"},
			"messages": []map[string]any{
				{"role": "user", "content": fmt.Sprintf(escalationPrompt, bundle)},
			},
		}
		text, err := c.messages(ctx, body)
		return text, 0, err
	}

	decls := make([]map[string]any, len(tools))
	for i, t := range tools {
		decls[i] = map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.InputSchema,
		}
	}
	messages := []any{
		map[string]any{"role": "user", "content": fmt.Sprintf(escalationPrompt, bundle) + escalationToolNote},
	}

	used := 0
	// Hard stop: budget tool calls can span at most budget tool-use turns,
	// plus the opening turn and the forced conclusion.
	for turn := 0; turn < budget+2; turn++ {
		body := map[string]any{
			"model":      c.cfg.EscalationModel,
			"max_tokens": 2048,
			"thinking":   map[string]any{"type": "adaptive"},
			"messages":   messages,
			"tools":      decls,
		}
		if used >= budget {
			// Tools stay declared (prior tool_use blocks require it) but the
			// model may no longer call them.
			body["tool_choice"] = map[string]any{"type": "none"}
		}
		resp, err := c.send(ctx, body)
		if err != nil {
			return "", used, err
		}
		if resp.StopReason != "tool_use" {
			for _, block := range resp.blocks() {
				if block.Type == "text" {
					return block.Text, used, nil
				}
			}
			return "", used, fmt.Errorf("no text content in escalation response (stop_reason=%s)", resp.StopReason)
		}

		// Replay the assistant turn exactly as received (thinking blocks
		// included, per API replay rules), then answer every tool_use.
		messages = append(messages, map[string]any{"role": "assistant", "content": resp.Content})
		var results []any
		for _, block := range resp.blocks() {
			if block.Type != "tool_use" {
				continue
			}
			result := map[string]any{"type": "tool_result", "tool_use_id": block.ID}
			if used >= budget {
				result["content"] = "tool budget exhausted"
				result["is_error"] = true
			} else {
				used++
				out, err := exec(ctx, block.Name, block.Input)
				if err != nil {
					result["content"] = err.Error()
					result["is_error"] = true
				} else {
					result["content"] = out
				}
			}
			results = append(results, result)
		}
		if used >= budget {
			results = append(results, map[string]any{"type": "text",
				"text": "The tool budget is exhausted. Give your final diagnosis now from the evidence gathered."})
		}
		messages = append(messages, map[string]any{"role": "user", "content": results})
	}
	return "", used, fmt.Errorf("escalation loop exceeded turn limit")
}

// messagesResponse keeps content blocks as raw JSON so tool-loop turns can be
// replayed to the API exactly as received (required for thinking blocks).
type messagesResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (mr *messagesResponse) blocks() []contentBlock {
	out := make([]contentBlock, 0, len(mr.Content))
	for _, raw := range mr.Content {
		var b contentBlock
		if json.Unmarshal(raw, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

func (c *Client) messages(ctx context.Context, body map[string]any) (string, error) {
	mr, err := c.send(ctx, body)
	if err != nil {
		return "", err
	}
	for _, block := range mr.blocks() {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text content in response (stop_reason=%s)", mr.StopReason)
}

func (c *Client) send(ctx context.Context, body map[string]any) (*messagesResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic API status %d: %s", resp.StatusCode, b)
	}
	var mr messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, err
	}
	return &mr, nil
}

package pipeline_test

// Phase 4 scenarios: the bounded read-only tool loop during Escalation.
// The Claude fake is scripted with tool_use turns; tool executions hit the
// same Loki/Prometheus/docker fakes and the real temp SQLite store.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
)

func lowTriage() claude.Triage {
	return claude.Triage{Summary: "unclear", LikelyCause: "?", Severity: "warning", Confidence: 0.3}
}

func toolUse(id, name string, input map[string]any) map[string]any {
	if input == nil {
		input = map[string]any{}
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// toolResults extracts every tool_result block sent back to the Claude fake.
// Each request replays the full conversation, so only the last escalation
// request is inspected — it contains each result exactly once.
func toolResults(calls []map[string]any) []map[string]any {
	esc := escalationRequests(calls)
	if len(esc) == 0 {
		return nil
	}
	var out []map[string]any
	msgs, _ := esc[len(esc)-1]["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			if block["type"] == "tool_result" {
				out = append(out, block)
			}
		}
	}
	return out
}

// escalationRequests filters recorded Claude calls down to non-triage ones.
func escalationRequests(calls []map[string]any) []map[string]any {
	var out []map[string]any
	for _, call := range calls {
		if _, isTriage := call["output_config"]; !isTriage {
			out = append(out, call)
		}
	}
	return out
}

func TestEscalationQueriesLogsOfNonTargetContainer(t *testing.T) {
	f := newFakes(t)
	f.setTriage(lowTriage())
	f.scriptEscalation(
		[]map[string]any{toolUse("tu1", "query_loki", map[string]any{"container": "postgres", "since_minutes": 120})},
	)
	p, _ := newPipeline(t, f)

	if err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	// The alert target is nginx; the tool call must have reached Loki asking
	// for postgres.
	var sawPostgres bool
	for _, q := range f.lokiQueriesSeen() {
		if strings.Contains(q, `container="postgres"`) {
			sawPostgres = true
		}
	}
	if !sawPostgres {
		t.Errorf("loki never received a postgres query; saw %v", f.lokiQueriesSeen())
	}

	results := toolResults(f.claudeCalls())
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	if results[0]["is_error"] == true {
		t.Errorf("tool result was an error: %v", results[0]["content"])
	}

	notifs := f.notifications()
	if len(notifs) != 1 || !strings.Contains(notifs[0].Body, "Deep diagnosis") {
		t.Errorf("final escalated diagnosis not notified: %+v", notifs)
	}
}

func TestToolBudgetEnforcedAndDiagnosisStillProduced(t *testing.T) {
	f := newFakes(t)
	f.setTriage(lowTriage())
	var turns [][]map[string]any
	for i := 0; i < 10; i++ { // fake keeps demanding tools well past the budget
		turns = append(turns, []map[string]any{toolUse(fmt.Sprintf("tu%d", i), "list_containers", nil)})
	}
	f.scriptEscalation(turns...)
	p, st := newPipeline(t, f)

	if err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	results := toolResults(f.claudeCalls())
	if len(results) != 5 {
		t.Errorf("tool calls executed = %d, want exactly the budget of 5", len(results))
	}
	esc := escalationRequests(f.claudeCalls())
	last := esc[len(esc)-1]
	tc, _ := last["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "none" {
		t.Errorf("final escalation request did not disable tool use: %v", last["tool_choice"])
	}

	notifs := f.notifications()
	if len(notifs) != 1 || !strings.Contains(notifs[0].Body, "Deep diagnosis") {
		t.Fatalf("budget exhaustion must still produce a notified diagnosis: %+v", notifs)
	}
	inc, _ := st.FindOpenByGroupKey("group1")
	if inc.EscalationOutput == "" {
		t.Error("no escalation diagnosis stored after budget exhaustion")
	}
}

func TestToolErrorFedBackAndEscalationConcludes(t *testing.T) {
	f := newFakes(t)
	f.setTriage(lowTriage())
	f.mu.Lock()
	f.lokiDown = true
	f.mu.Unlock()
	f.scriptEscalation(
		[]map[string]any{toolUse("tu1", "query_loki", map[string]any{"container": "postgres"})},
	)
	p, _ := newPipeline(t, f)

	if err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	results := toolResults(f.claudeCalls())
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	if results[0]["is_error"] != true {
		t.Errorf("loki failure not marked as error tool result: %v", results[0])
	}
	content, _ := results[0]["content"].(string)
	if !strings.Contains(content, "loki") {
		t.Errorf("error result does not name the failing source: %q", content)
	}

	notifs := f.notifications()
	if len(notifs) != 1 || !strings.Contains(notifs[0].Body, "Deep diagnosis") {
		t.Errorf("escalation did not conclude after tool error: %+v", notifs)
	}
}

func TestTriageNeverDeclaresTools(t *testing.T) {
	f := newFakes(t)
	f.setTriage(lowTriage()) // force escalation so both request kinds occur
	p, _ := newPipeline(t, f)

	if err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	calls := f.claudeCalls()
	if _, isTriage := calls[0]["output_config"]; !isTriage {
		t.Fatal("first call is not the triage request")
	}
	if _, hasTools := calls[0]["tools"]; hasTools {
		t.Error("triage request declares tools")
	}
}

func TestBudgetZeroRevertsToSingleShotEscalation(t *testing.T) {
	f := newFakes(t)
	f.setTriage(lowTriage())
	p, _ := newPipeline(t, f)
	p.ToolBudget = 0

	if err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	calls := f.claudeCalls()
	if len(calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (triage + single-shot escalation)", len(calls))
	}
	if _, hasTools := calls[1]["tools"]; hasTools {
		t.Error("budget 0 escalation still declares tools")
	}
	notifs := f.notifications()
	if len(notifs) != 1 || !strings.Contains(notifs[0].Body, "Deep diagnosis") {
		t.Errorf("single-shot escalation not notified: %+v", notifs)
	}
}

func TestGetIncidentsReturnsHistoryCreatedThroughPipeline(t *testing.T) {
	f := newFakes(t)
	p, _ := newPipeline(t, f)
	ctx := context.Background()

	// A prior episode created and resolved through the pipeline itself.
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // RFC3339 second resolution
	if err := p.HandleWebhook(ctx, webhookResolved("group1")); err != nil {
		t.Fatal(err)
	}

	f.setTriage(lowTriage())
	f.scriptEscalation(
		[]map[string]any{toolUse("tu1", "get_incidents", map[string]any{"target": "nginx"})},
	)
	if err := p.HandleWebhook(ctx, webhookFiring("group2", "nginx")); err != nil {
		t.Fatal(err)
	}

	results := toolResults(f.claudeCalls())
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	content, _ := results[0]["content"].(string)
	if !strings.Contains(content, "bad config after last edit") {
		t.Errorf("incident history missing prior diagnosis verdict: %q", content)
	}
	if !strings.Contains(content, "resolved after") {
		t.Errorf("incident history missing resolution outcome: %q", content)
	}
}

func TestToolResultsRespectByteCap(t *testing.T) {
	f := newFakes(t)
	f.setTriage(lowTriage())
	f.mu.Lock()
	f.lokiLines = nil
	for i := 0; i < 400; i++ {
		f.lokiLines = append(f.lokiLines, fmt.Sprintf("line-%03d some verbose log output padding padding padding padding padding padding", i))
	}
	f.mu.Unlock()
	f.scriptEscalation(
		[]map[string]any{toolUse("tu1", "query_loki", map[string]any{"container": "postgres"})},
	)
	p, _ := newPipeline(t, f)

	if err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	results := toolResults(f.claudeCalls())
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	content, _ := results[0]["content"].(string)
	if !strings.Contains(content, "truncated") {
		t.Error("oversized tool result missing truncation note")
	}
	if len(content) > 17000 { // cap 16384 + note
		t.Errorf("tool result %d bytes, exceeds the byte cap", len(content))
	}
	if !strings.Contains(content, "line-399") {
		t.Error("newest line missing — truncation kept old data instead of newest")
	}
}

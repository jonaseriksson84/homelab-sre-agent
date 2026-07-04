package pipeline_test

// Tests drive the top-level pipeline with every dependency (Loki, Prometheus,
// docker proxy, Claude API, ntfy) faked at the HTTP boundary, and a real
// temp-file SQLite store. Assertions cover only operator-observable behavior:
// the Diagnosis, the stored Incident, and the requests ntfy/Claude received.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
)

// --- fakes ---

type ntfyRequest struct {
	Title    string
	Priority string
	Body     string
}

type fakes struct {
	loki, prom, docker, anthropic, ntfy *httptest.Server

	mu             sync.Mutex
	ntfyRequests   []ntfyRequest
	claudeRequests []map[string]any
	triage         claude.Triage
	escalation     string
	lokiLines      []string
	lokiDown       bool
}

func newFakes(t *testing.T) *fakes {
	f := &fakes{
		triage: claude.Triage{
			Summary:     "nginx is crash-looping because its config references a missing upstream.",
			LikelyCause: "bad config after last edit",
			Severity:    "warning",
			Confidence:  0.9,
		},
		escalation: "Deep diagnosis: the upstream host in nginx.conf no longer resolves.",
		lokiLines:  []string{"2026/07/04 nginx: [emerg] host not found in upstream"},
	}

	f.loki = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
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
				{"values": [][2]any{{1e9, "42.5"}, {1e9, "43.1"}}},
			}},
		})
	}))

	f.docker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"Names": []string{"/nginx"}, "State": "restarting", "Status": "Restarting (1) 5s ago", "Image": "nginx:latest"},
			{"Names": []string{"/postgres"}, "State": "running", "Status": "Up 3 days", "Image": "postgres:16"},
		})
	}))

	f.anthropic = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		f.mu.Lock()
		f.claudeRequests = append(f.claudeRequests, req)
		triage := f.triage
		escalation := f.escalation
		f.mu.Unlock()

		var text string
		if _, isTriage := req["output_config"]; isTriage {
			b, _ := json.Marshal(triage)
			text = string(b)
		} else {
			text = escalation
		}
		json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": text}},
			"stop_reason": "end_turn",
		})
	}))

	f.ntfy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.ntfyRequests = append(f.ntfyRequests, ntfyRequest{
			Title:    r.Header.Get("Title"),
			Priority: r.Header.Get("Priority"),
			Body:     string(body),
		})
		f.mu.Unlock()
	}))

	t.Cleanup(func() {
		for _, s := range []*httptest.Server{f.loki, f.prom, f.docker, f.anthropic, f.ntfy} {
			s.Close()
		}
	})
	return f
}

func (f *fakes) notifications() []ntfyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ntfyRequest(nil), f.ntfyRequests...)
}

func (f *fakes) claudeCalls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.claudeRequests...)
}

func (f *fakes) setTriage(tr claude.Triage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triage = tr
}

// --- helpers moved to setup_test.go ---

func TestManualDiagnoseStoresIncidentAndNeverNotifies(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)

	d, err := p.DiagnoseManual(context.Background(), "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text(), "crash-looping") {
		t.Errorf("diagnosis text missing summary: %q", d.Text())
	}
	inc, err := st.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Source != "manual" || inc.Target != "nginx" {
		t.Errorf("incident = source %q target %q, want manual/nginx", inc.Source, inc.Target)
	}
	if inc.Notified {
		t.Error("manual incident must not be notified")
	}
	if len(f.notifications()) != 0 {
		t.Errorf("manual diagnose sent %d notifications, want 0", len(f.notifications()))
	}
	if inc.ModelUsed != "test-haiku" {
		t.Errorf("model used = %q, want test-haiku (no escalation at 0.9 confidence)", inc.ModelUsed)
	}
}

func TestLowConfidenceEscalatesAndNotifiesOnce(t *testing.T) {
	f := newFakes(t)
	f.setTriage(claude.Triage{Summary: "unclear", LikelyCause: "?", Severity: "warning", Confidence: 0.3})
	p, st := newPipeline(t, f)

	err := p.HandleWebhook(context.Background(), webhookFiring("group1", "nginx"))
	if err != nil {
		t.Fatal(err)
	}

	calls := f.claudeCalls()
	if len(calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (triage + escalation)", len(calls))
	}
	if calls[0]["model"] != "test-haiku" || calls[1]["model"] != "test-opus" {
		t.Errorf("models = %v, %v; want test-haiku then test-opus", calls[0]["model"], calls[1]["model"])
	}

	notifs := f.notifications()
	if len(notifs) != 1 {
		t.Fatalf("notifications = %d, want exactly 1", len(notifs))
	}
	if !strings.Contains(notifs[0].Body, "Deep diagnosis") {
		t.Errorf("notification carried triage, not escalated diagnosis: %q", notifs[0].Body)
	}

	inc, _ := st.FindOpenByGroupKey("group1")
	if inc == nil {
		t.Fatal("no open incident stored")
	}
	if inc.EscalationOutput == "" || inc.ModelUsed != "test-opus" {
		t.Errorf("escalation not recorded: output=%q model=%q", inc.EscalationOutput, inc.ModelUsed)
	}
}

func TestRepeatFiringBumpsLastSeenWithoutNewDiagnosis(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)
	ctx := context.Background()

	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	callsBefore := len(f.claudeCalls())
	notifsBefore := len(f.notifications())
	first, _ := st.FindOpenByGroupKey("group1")

	time.Sleep(1100 * time.Millisecond) // RFC3339 second resolution
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	if n := len(f.claudeCalls()); n != callsBefore {
		t.Errorf("repeat firing made %d extra claude calls", n-callsBefore)
	}
	if n := len(f.notifications()); n != notifsBefore {
		t.Errorf("repeat firing sent %d extra notifications", n-notifsBefore)
	}
	after, _ := st.FindOpenByGroupKey("group1")
	if after.ID != first.ID {
		t.Errorf("repeat firing created a new incident (%d → %d)", first.ID, after.ID)
	}
	if !after.LastSeen.After(first.LastSeen) {
		t.Errorf("last_seen not bumped: %v → %v", first.LastSeen, after.LastSeen)
	}
}

func TestResolvedClosesIncidentWithLowPriorityPing(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)
	ctx := context.Background()

	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	inc, _ := st.FindOpenByGroupKey("group1")

	if err := p.HandleWebhook(ctx, webhookResolved("group1")); err != nil {
		t.Fatal(err)
	}

	if open, _ := st.FindOpenByGroupKey("group1"); open != nil {
		t.Error("incident still open after resolved webhook")
	}
	closed, _ := st.Get(inc.ID)
	if closed.Status != "resolved" || closed.ResolvedAt == nil {
		t.Errorf("incident not marked resolved: status=%q resolved_at=%v", closed.Status, closed.ResolvedAt)
	}

	notifs := f.notifications()
	last := notifs[len(notifs)-1]
	if last.Priority != "2" {
		t.Errorf("resolve ping priority = %s, want 2 (low)", last.Priority)
	}
	if !strings.Contains(last.Body, "resolved after") {
		t.Errorf("resolve ping missing duration: %q", last.Body)
	}
}

func TestFlapCreatesNewIncident(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)
	ctx := context.Background()

	p.HandleWebhook(ctx, webhookFiring("group1", "nginx"))
	first, _ := st.FindOpenByGroupKey("group1")
	p.HandleWebhook(ctx, webhookResolved("group1"))
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	second, _ := st.FindOpenByGroupKey("group1")
	if second == nil {
		t.Fatal("refire did not create an open incident")
	}
	if second.ID == first.ID {
		t.Error("refire reopened the resolved incident instead of creating a new one")
	}
	old, _ := st.Get(first.ID)
	if old.Status != "resolved" {
		t.Errorf("original incident status = %q, want resolved", old.Status)
	}
}

func TestMissingContainerLabelFuzzyMatchesRunningContainer(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)

	wh := pipeline_Webhook("group1", map[string]string{"alertname": "NginxDown"})
	if err := p.HandleWebhook(context.Background(), wh); err != nil {
		t.Fatal(err)
	}
	inc, _ := st.FindOpenByGroupKey("group1")
	if inc.Target != "nginx" {
		t.Errorf("target = %q, want fuzzy-matched nginx", inc.Target)
	}
}

func TestNoTargetStillDiagnosesHostLevel(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)

	wh := pipeline_Webhook("group1", map[string]string{"alertname": "HostHighMemory"})
	if err := p.HandleWebhook(context.Background(), wh); err != nil {
		t.Fatal(err)
	}
	inc, _ := st.FindOpenByGroupKey("group1")
	if inc == nil {
		t.Fatal("no incident for host-level alert")
	}
	if inc.Target != "" {
		t.Errorf("target = %q, want empty (host-level)", inc.Target)
	}
	if len(f.notifications()) != 1 {
		t.Errorf("host-level alert sent %d notifications, want 1", len(f.notifications()))
	}
}

func TestLokiDownDegradesGracefully(t *testing.T) {
	f := newFakes(t)
	f.mu.Lock()
	f.lokiDown = true
	f.mu.Unlock()
	p, _ := newPipeline(t, f)

	d, err := p.DiagnoseManual(context.Background(), "nginx")
	if err != nil {
		t.Fatalf("diagnosis failed when loki down: %v", err)
	}
	if d.Text() == "" {
		t.Error("empty diagnosis when loki down")
	}
	// The bundle Claude saw must note the gap instead of omitting it silently.
	calls := f.claudeCalls()
	msgs := calls[0]["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "SOURCE UNAVAILABLE") {
		t.Error("bundle does not note the unavailable loki source")
	}
}

func TestLogByteBudgetTruncatesKeepingNewestLines(t *testing.T) {
	f := newFakes(t)
	f.mu.Lock()
	f.lokiLines = nil
	for i := 0; i < 200; i++ {
		f.lokiLines = append(f.lokiLines, fmt.Sprintf("line-%03d some log output padding padding padding", i))
	}
	f.mu.Unlock()
	p, _ := newPipeline(t, f) // budget set to 2048 in newPipeline

	if _, err := p.DiagnoseManual(context.Background(), "nginx"); err != nil {
		t.Fatal(err)
	}
	calls := f.claudeCalls()
	msgs := calls[0]["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "truncated") {
		t.Error("bundle missing truncation note")
	}
	if !strings.Contains(content, "line-199") {
		t.Error("newest line missing from bundle")
	}
	if strings.Contains(content, "line-000") {
		t.Error("oldest line present — budget kept old lines instead of newest")
	}
}

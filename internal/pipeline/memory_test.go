package pipeline_test

// Incident Memory scenarios: history is created through the pipeline itself
// (fire → resolve → refire, manual runs) wherever possible; direct store
// seeding is used only to age rows past the window/cap, since timestamps
// aren't reachable through the public surface. Assertions read the bundle
// the Claude fake received.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
)

// bundleOf extracts the Context Bundle text from a recorded Claude request.
func bundleOf(t *testing.T, call map[string]any) string {
	t.Helper()
	msgs, ok := call["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatal("claude call has no messages")
	}
	return msgs[0].(map[string]any)["content"].(string)
}

func lastBundle(t *testing.T, f *fakes) string {
	t.Helper()
	calls := f.claudeCalls()
	if len(calls) == 0 {
		t.Fatal("no claude calls recorded")
	}
	return bundleOf(t, calls[len(calls)-1])
}

func TestFlapSecondIncidentCarriesMemoryOfFirst(t *testing.T) {
	f := newFakes(t)
	p, _ := newPipeline(t, f)
	ctx := context.Background()

	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleWebhook(ctx, webhookResolved("group1")); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	bundle := lastBundle(t, f)
	if !strings.Contains(bundle, "## Incident Memory") {
		t.Fatal("refire bundle has no Incident Memory section")
	}
	// The prior verdict is the first incident's triage likely-cause.
	if !strings.Contains(bundle, "bad config after last edit") {
		t.Errorf("memory missing prior diagnosis verdict:\n%s", bundle)
	}
	if !strings.Contains(bundle, "resolved after") {
		t.Errorf("memory missing time-to-resolve:\n%s", bundle)
	}
	// Exactly one prior: the incident under diagnosis must not list itself.
	if n := strings.Count(bundle, "ago (alertmanager)"); n != 1 {
		t.Errorf("memory entries = %d, want 1 (self excluded):\n%s", n, bundle)
	}
}

func TestMemoryMatchesOnAlertnameWithoutTarget(t *testing.T) {
	f := newFakes(t)
	p, _ := newPipeline(t, f)
	ctx := context.Background()

	// Host-level alert: no container label, no fuzzy match → target "".
	wh := pipeline_Webhook("group1", map[string]string{"alertname": "HostHighMemory"})
	if err := p.HandleWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleWebhook(ctx, webhookResolved("group1")); err != nil {
		t.Fatal(err)
	}
	wh2 := pipeline_Webhook("group2", map[string]string{"alertname": "HostHighMemory"})
	if err := p.HandleWebhook(ctx, wh2); err != nil {
		t.Fatal(err)
	}

	bundle := lastBundle(t, f)
	if !strings.Contains(bundle, "HostHighMemory") || !strings.Contains(bundle, "ago (alertmanager)") {
		t.Errorf("alertname-matched memory missing from host-level bundle:\n%s", bundle)
	}
}

func TestUnrelatedIncidentsExcludedAndNoPriorsLineRenders(t *testing.T) {
	f := newFakes(t)
	p, _ := newPipeline(t, f)
	ctx := context.Background()

	// A container incident, then an unrelated host-level alert.
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	wh := pipeline_Webhook("group2", map[string]string{"alertname": "HostHighMemory"})
	if err := p.HandleWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}

	bundle := lastBundle(t, f)
	if strings.Contains(bundle, "ago (alertmanager)") {
		t.Errorf("unrelated incident leaked into memory:\n%s", bundle)
	}
	if !strings.Contains(bundle, "no prior incidents") {
		t.Errorf("bundle missing explicit no-priors line:\n%s", bundle)
	}
}

func TestMemoryCapKeepsNewestEntries(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)
	now := time.Now()

	// Seven matching priors, seeded with distinct ages (oldest = Alert000).
	for i := 0; i < 7; i++ {
		seedIncident(t, st, "nginx", fmt.Sprintf("Alert%03d", i), now.Add(-time.Duration(7-i)*24*time.Hour))
	}
	if _, err := p.DiagnoseManual(context.Background(), "nginx"); err != nil {
		t.Fatal(err)
	}

	bundle := lastBundle(t, f)
	if n := strings.Count(bundle, "ago (alertmanager)"); n != 5 {
		t.Errorf("memory entries = %d, want 5 (capped):\n%s", n, bundle)
	}
	if strings.Contains(bundle, "Alert000") || strings.Contains(bundle, "Alert001") {
		t.Errorf("cap kept oldest entries instead of newest:\n%s", bundle)
	}
	// Most recent first.
	if strings.Index(bundle, "Alert006") > strings.Index(bundle, "Alert002") {
		t.Errorf("memory not ordered most-recent-first:\n%s", bundle)
	}
}

func TestMemoryWindowExcludesOldRows(t *testing.T) {
	f := newFakes(t)
	p, st := newPipeline(t, f)

	seedIncident(t, st, "nginx", "AncientAlert", time.Now().Add(-31*24*time.Hour))
	if _, err := p.DiagnoseManual(context.Background(), "nginx"); err != nil {
		t.Fatal(err)
	}

	bundle := lastBundle(t, f)
	if strings.Contains(bundle, "AncientAlert") {
		t.Errorf("row older than the window included in memory:\n%s", bundle)
	}
	if !strings.Contains(bundle, "no prior incidents") {
		t.Errorf("bundle missing no-priors line when history is stale:\n%s", bundle)
	}
}

func TestManualRunReceivesMemoryAndOpenPriorRendersStillOpen(t *testing.T) {
	f := newFakes(t)
	p, _ := newPipeline(t, f)
	ctx := context.Background()

	// A webhook incident left open, then a manual investigation of the same target.
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DiagnoseManual(ctx, "nginx"); err != nil {
		t.Fatal(err)
	}

	bundle := lastBundle(t, f)
	if !strings.Contains(bundle, "ago (alertmanager)") {
		t.Errorf("manual run bundle missing memory of prior incident:\n%s", bundle)
	}
	if !strings.Contains(bundle, "STILL OPEN") {
		t.Errorf("open prior not rendered as still open:\n%s", bundle)
	}
}

func TestMemoryLimitZeroOmitsSection(t *testing.T) {
	f := newFakes(t)
	p, _ := newPipeline(t, f)
	p.MemoryMaxEntries = 0
	ctx := context.Background()

	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleWebhook(ctx, webhookResolved("group1")); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleWebhook(ctx, webhookFiring("group1", "nginx")); err != nil {
		t.Fatal(err)
	}

	if bundle := lastBundle(t, f); strings.Contains(bundle, "Incident Memory") {
		t.Errorf("limit 0 still rendered the memory section:\n%s", bundle)
	}
}

// seedIncident writes a resolved prior directly — only for backdating
// created_at, which the public surface can't do.
func seedIncident(t *testing.T, st *store.Store, target, alertName string, createdAt time.Time) {
	t.Helper()
	resolved := createdAt.Add(5 * time.Minute)
	inc := &store.Incident{
		Source:     "alertmanager",
		GroupKey:   "seed-" + alertName,
		AlertNames: alertName,
		Target:     target,
		Status:     "open",
		CreatedAt:  createdAt,
		LastSeen:   createdAt,
	}
	if err := st.Create(inc); err != nil {
		t.Fatal(err)
	}
	if err := st.Resolve(inc.ID, resolved); err != nil {
		t.Fatal(err)
	}
}

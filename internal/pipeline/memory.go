// Incident Memory: prior Incidents from the store rendered as compact
// one-liners and appended to the Context Bundle, so recurrences are diagnosed
// as recurrences. Flaps create new Incidents; memory is what connects them.
package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/claude"
	"github.com/jonaseriksson84/homelab-sre-agent/internal/store"
)

// memoryVerdictMax caps each prior diagnosis one-liner so a verbose
// escalation can't crowd out the rest of the bundle.
const memoryVerdictMax = 160

// incidentMemory renders the "Incident Memory" bundle section for the
// incident under diagnosis (excludeID; 0 when none exists yet). Returns ""
// when the feature is disabled. A store failure degrades to a SOURCE
// UNAVAILABLE note like any other bundle source — never fatal.
func (p *Pipeline) incidentMemory(target string, alertNames []string, excludeID int64, now time.Time) string {
	if p.MemoryMaxEntries <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Incident Memory (prior incidents from this agent's history)\n")

	since := now.AddDate(0, 0, -p.MemoryWindowDays)
	priors, err := p.Store.FindRecentMatching(target, alertNames, excludeID, since, p.MemoryMaxEntries)
	if err != nil {
		p.Log.Error("incident memory lookup failed", "error", err)
		fmt.Fprintf(&b, "SOURCE UNAVAILABLE: incident store: %v\n", err)
		return b.String()
	}
	if len(priors) == 0 {
		fmt.Fprintf(&b, "(no prior incidents match this target or alert in the last %d days)\n", p.MemoryWindowDays)
		return b.String()
	}
	for _, inc := range priors {
		b.WriteString(memoryLine(inc, now))
	}
	return b.String()
}

// memoryLine renders one prior Incident: age, source, what fired, severity,
// the final Diagnosis verdict (escalated when escalation ran), and outcome.
func memoryLine(inc *store.Incident, now time.Time) string {
	what := inc.AlertNames
	if what == "" {
		what = inc.Target
	}

	verdict := "no diagnosis recorded"
	severity := ""
	if inc.TriageOutput != "" {
		var tr claude.Triage
		if json.Unmarshal([]byte(inc.TriageOutput), &tr) == nil {
			severity = tr.Severity
			verdict = tr.LikelyCause
		}
	}
	if inc.EscalationOutput != "" {
		verdict = inc.EscalationOutput
	}
	verdict = truncateLine(verdict, memoryVerdictMax)
	if severity != "" {
		severity = " [" + severity + "]"
	}

	outcome := "STILL OPEN"
	if inc.ResolvedAt != nil {
		outcome = "resolved after " + inc.ResolvedAt.Sub(inc.CreatedAt).Round(time.Minute).String()
	}
	return fmt.Sprintf("- %s ago (%s)%s %s: %s — %s\n",
		relAge(now.Sub(inc.CreatedAt)), inc.Source, severity, what, verdict, outcome)
}

func relAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

func truncateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ") // flatten newlines/runs of space
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

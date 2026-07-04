# Homelab SRE Agent

A Go service that auto-diagnoses homelab incidents from Alertmanager webhooks using the Claude API, and exposes read-only homelab status via MCP.

## Language

**Alert**:
A single firing condition from Alertmanager (one `alerts[]` entry in a webhook payload). Alerts are inputs; they are never diagnosed individually.

**Incident**:
One diagnosable episode, with a source: an Alertmanager firing episode (identified by the payload's `groupKey` while `firing`) or a manual CLI invocation against a target. An incident gets exactly one row in the store and one diagnosis; Alertmanager incidents drive notifications, manual ones don't.
_Avoid_: alert (for the grouped thing), issue, event

**Source**:
Where an incident came from: `alertmanager` or `manual`. Both run the identical gather → triage → escalate pipeline.

**Open / Resolved**:
An incident is Open from its first `firing` webhook until Alertmanager sends `resolved` for the same group, which closes it. Repeat firing webhooks only bump `last_seen` — no new diagnosis, no new notification. Resolved incidents are never reopened: if the same group fires again later, that is a new incident.

**Target**:
The container(s) an incident is about, resolved from alert labels (a `container` label by convention; fuzzy name match as fallback). An incident with no resolvable target is still diagnosed, using host-level context only.

**Context Bundle**:
The evidence gathered for one diagnosis: the target's Loki logs around the incident, a fixed panel of Prometheus queries (per-target CPU/memory/restarts, node CPU/memory/disk/load, downsampled), and Docker container states. Deterministic and size-bounded; ad-hoc queries belong to agentic tool use, not the bundle.
_Avoid_: context (alone — too overloaded)

**Diagnosis**:
The Claude-produced plain-language explanation of an incident, generated from its Context Bundle. Includes a structured self-reported confidence.
_Avoid_: summary, analysis

**Incident Memory**:
Recent related history injected into a Context Bundle: the last few incidents sharing the new incident's target or alertname, as compact one-liners (when, prior diagnosis, time to resolve). Turns repeat failures into pattern signals.
_Avoid_: history (alone)

**Triage / Escalation**:
Triage is the always-first, single-shot diagnosis by the cheap model on the Context Bundle. Escalation is re-running the same bundle through the expensive model when triage confidence falls below threshold; only the escalated diagnosis is notified. Escalation (and only escalation) may use the Toolset in a bounded loop.

**Toolset**:
The single registry of read-only homelab query functions (Loki, Prometheus, container inspection, incident history) exposed through two frontends: Claude API tool use during escalation, and the MCP server for chat. There is exactly one implementation of each tool.

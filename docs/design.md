# Design Decisions — Homelab SRE Agent

Full record of the design decisions from the initial planning session (2026-07-04). The glossary lives in `CONTEXT.md`; hard-to-reverse decisions have ADRs in `docs/adr/`. This doc captures everything else so later PRDs (phases 3–5) can be written without the original conversation.

## Phases

1. CLI tool: `sre-agent diagnose <container>` — manual, proves the prompt works
2. Webhook receiver + notifications — automated
3. Incident memory — SQLite history fed back into context
4. Agentic tool use — read-only tools on demand during escalation
5. MCP server exposing read-only homelab tools → chat about homelab status

## Incident model

- **Incident** = one diagnosable episode with a `source`: `alertmanager` (one firing episode of an Alertmanager group, keyed by the payload `groupKey` while status is `firing`) or `manual` (CLI invocation against a target). One store row, one diagnosis each.
- Lifecycle: first firing webhook creates the incident and triggers the one diagnosis + notification. Repeat firing webhooks only bump `last_seen` — no new Claude call, no new ping. The `resolved` webhook closes the incident (low-priority "resolved after Xm" ntfy ping).
- Resolved incidents are never reopened. A flap (same group refires after resolving) is a **new incident**; flap awareness comes from Incident Memory, not from reopening rows.
- Manual incidents run the identical pipeline but are not notified (the user is looking at the output).

## Targeting

- Primary: a `container` label on the alert (enforce via Alertmanager rule conventions).
- Fallback: fuzzy match of alertname/labels against running container names.
- No resolvable target → still diagnose, using host-level context only. Never fail a diagnosis for want of a target.

## Context Bundle

Deterministic and size-bounded. Contents:

- **Loki logs**: the target's logs around the incident (±15 min window), fetched with a line limit then enforced to a byte budget (~30–50 KB per target), keeping the most recent lines before the alert fired, with a "N lines truncated" note. No smart filtering.
- **Prometheus**: a hardcoded panel of PromQL queries — per-target CPU/memory/restart-count (cAdvisor) + node-level CPU/memory/disk/load — each as a small downsampled series (~15 points over 30 min) rendered as compact text.
- **Docker**: container states from the socket proxy.
- **Incident Memory** (phase 3): last ~5 incidents matching the new incident's target OR alertname within ~30 days, injected as compact one-liners (when, prior diagnosis verdict, time to resolve).

Ad-hoc queries never go in the bundle — they belong to the escalation tool loop.

## Triage → Escalation

- **Triage**: always-first, single-shot call on the Context Bundle using `claude-haiku-4-5`. Must return a structured confidence field (via structured outputs / `output_config.format`, not prose parsing — prefills don't exist on current models).
- **Escalation**: when triage confidence falls below threshold (or explicit "insufficient evidence"), re-run the same bundle on `claude-opus-4-8`. Only the escalated diagnosis is notified.
- **Tool use (phase 4)**: only the escalated call gets a bounded tool loop (~5 calls max). Triage stays single-shot.

## Toolset (phases 4–5)

One registry of read-only query functions — `query_loki`, `query_prometheus`, `inspect_container`/`list_containers`, `get_incidents` — with exactly one implementation of each and two frontends: Claude API tool use during escalation, and the MCP server for chat.

## Notifications

- ntfy only, behind a small Notifier interface (Discord is a possible later drop-in).
- Notify on new alertmanager-sourced diagnosis; low-priority ping on resolve; manual incidents never notify.

## Security

- **Docker**: agent never mounts the real socket — talks to a GET-only docker-socket-proxy container over tcp (ADR-0001). Enforcement lives outside the LLM-adjacent process.
- **MCP**: streamable HTTP, bound/firewalled to the Tailscale interface, deliberately no app-level auth (ADR-0002).
- **Webhook endpoint**: LAN/docker-network-only, no auth — same trust boundary as Alertmanager itself.
- Read-only throughout; no auto-remediation.

## Architecture

- One Go module, one binary, subcommands: `sre-agent diagnose <container>`, `sre-agent serve` (webhook + MCP in one process). Internal packages (gather, diagnose, store, notify) shared by all entry points. One Docker image on Unraid.
- SQLite for the incident store; single process so no cross-process locking concerns.
- Config via env vars.

## Testing

Single seam at the outbound HTTP boundary: every dependency (Loki, Prometheus, docker socket proxy, Claude API, ntfy) is HTTP with a configurable base URL; tests point them at `httptest` fakes and drive the top-level pipeline. SQLite is tested with real temp databases, not mocked.

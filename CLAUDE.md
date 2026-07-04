# Homelab SRE Agent

A Go service on Unraid that auto-diagnoses homelab incidents from Alertmanager webhooks using the Claude API, plus an MCP server for chatting about homelab status. See `CONTEXT.md` for the domain glossary, `docs/adr/` for architectural decisions, and `docs/deployment.md` for the live Tower/Unraid environment (Alertmanager already sends webhooks to `sre-agent:8080/webhook`).

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via the `gh` CLI; external PRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Canonical defaults: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

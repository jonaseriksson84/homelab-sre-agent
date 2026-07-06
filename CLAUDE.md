# Homelab SRE Agent

A Go service that auto-diagnoses homelab incidents from Alertmanager webhooks using the Claude API, plus an MCP server for chatting about homelab status. See `CONTEXT.md` for the domain glossary, `docs/adr/` for architectural decisions, and `docs/setup.md` for deployment. Operator-local records (`implementation-notes.md`, `docs/deployment.tower.md`) are gitignored — read and update them when present, but never commit them.

## Build & test

```bash
go build ./...   # single binary: sre-agent <diagnose <container> | serve>
go test ./...    # all deps faked at the HTTP seam; no API key or network needed
```

Docker image: `docker build -t sre-agent .` (static, CGO disabled; `--build-arg VERSION=x.y.z` stamps releases). Adding a config env var requires updating the Unraid template (`unraid/sre-agent.xml`) — the root packaging test enforces this.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via the `gh` CLI; external PRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Canonical defaults: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

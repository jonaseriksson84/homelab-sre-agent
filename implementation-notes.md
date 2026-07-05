# Implementation Notes — issue #1 (phases 1–2)

Running log kept during implementation. PRD: GitHub issue #1. Design: `docs/design.md`.

## Progress

- 2026-07-04: `go mod init github.com/jonaseriksson84/homelab-sre-agent` (go 1.26).
- Package layout: `main.go` (subcommands `diagnose`, `serve`) + `internal/config`, `internal/gather`, `internal/claude`, `internal/store`, `internal/notify`, `internal/pipeline`, `internal/server`.
- 2026-07-04: full phases 1–2 pipeline implemented and green: gather (Loki ±15m + byte budget, Prometheus panel downsampled, docker states, per-source degradation), claude (Haiku triage via `output_config.format` JSON schema, Opus escalation with adaptive thinking), store (SQLite, open-incident-per-groupKey dedup), notify (ntfy, severity→priority), pipeline (lifecycle: create/bump/resolve/flap, manual never notifies), server (async webhook, 202, /healthz).
- Tests: 11 passing — 9 pipeline scenarios (manual no-notify, low-confidence escalation notified once, repeat-firing bump, resolved low-priority ping, flap→new incident, fuzzy target match, host-level no-target, Loki-down degradation noted in bundle, byte-budget truncation keeps newest) + 2 server (async 202 creates incident, 400 on invalid payload). All deps faked at the HTTP seam; real temp SQLite.
- Dockerfile (static build, non-root) + `docker-compose.example.yml` matching the deployment contract in `docs/deployment.md`, including the ADR-0001 socket proxy.

## Deviations

- **Claude API via raw net/http instead of the official Go SDK.** The PRD's single testing seam is "every dependency is HTTP with a configurable base URL" pointed at `httptest` fakes, and config explicitly lists the Claude endpoint as an env var. Raw HTTP keeps that seam uniform and the module dependency-free except for the SQLite driver. Conservative in the sense of fewest moving parts; revisit if we need SDK-only features (streaming, tool runner in phase 4).
- **SQLite driver: `modernc.org/sqlite` (pure Go, no cgo).** Not specified in the PRD. Chosen so the single Docker image can be built with CGO_ENABLED=0 and cross-compiled for Unraid without a C toolchain.
- **Manual incidents are stored already-resolved.** The PRD says manual runs become Incidents with source `manual`; it doesn't define their lifecycle. A CLI run is a one-shot episode with no resolved webhook coming, so leaving it open would strand rows forever. Conservative: create + record diagnosis + resolve immediately.
- **Escalation failure keeps the triage diagnosis.** Not specified in the PRD. If the Opus call errors, notifying the (low-confidence) triage result beats notifying nothing; the store shows `model_used` = triage model so the degradation is auditable.
- **Loki log selector is `{container_name="<target>"}`.** The PRD says "the target's logs"; the label is configurable via `SRE_LOKI_CONTAINER_LABEL`. Originally defaulted to `container`, but the live smoke test (2026-07-05) showed promtail on Tower exposes `container_name` (plain names, no `/` prefix) — default updated to match.
- **Structured-output schema can't use `minimum`/`maximum` on numbers.** The live API rejects them with a 400 (the fakes didn't validate schemas, so tests missed it). The confidence 0–1 range moved into the field description and is clamped after parsing.

- 2026-07-04: Docker image build verified on Tower (`git archive | ssh tower docker build -` → `sre-agent:dev`); binary runs and fails cleanly on missing `ANTHROPIC_API_KEY`. Local Docker daemon wasn't running, hence the remote build.

## Open questions / to verify on the live stack

- ~~Confirm the Loki stream label promtail uses for container names on Tower.~~ Resolved 2026-07-05: it's `container_name`.
- ~~Smoke-test `diagnose` against tailnet endpoints.~~ Done 2026-07-05: `diagnose grafana` from a dev Mac ran the full pipeline (bundle → Haiku triage → printed diagnosis, confidence 0.85). Docker states were unavailable as expected — the socket proxy deploys with the agent.

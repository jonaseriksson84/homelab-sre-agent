# homelab-sre-agent

When something breaks in the homelab, Alertmanager tells you *that* it broke — this agent tells you *why*. A single Go binary that receives Alertmanager webhooks, gathers logs, metrics, and container state around the incident, asks Claude what most likely went wrong, and pushes a plain-language diagnosis to your phone before you've even opened Grafana.

```
Alertmanager ──webhook──▶ sre-agent ──▶ gather Context Bundle ──▶ Claude triage (Haiku)
                                          │  Loki logs ±15m          │ low confidence?
                                          │  Prometheus panel        ▼
                                          │  Docker states        escalate (Opus)
                                          ▼                          │
                                       SQLite ◀── Incident ──▶ ntfy ─┘
```

## How it works

- **Incident lifecycle** — the first firing webhook for an Alertmanager group creates an Incident, runs one diagnosis, and sends one notification. Repeat firings only bump `last_seen` (no extra Claude calls, no extra pings). The resolved webhook closes the Incident with a low-priority "resolved after Xm" ping. A group that flaps becomes a new Incident.
- **Targeting** — the alert's `container` label picks the diagnosis Target; without one, alert labels are fuzzy-matched against running container names; failing that, the diagnosis runs on host-level context alone. No incident is ever dropped for want of a target.
- **Context Bundle** — deterministic and size-bounded: the target's Loki logs (±15 min, byte-budgeted keeping the newest lines), a fixed panel of downsampled Prometheus queries, and Docker container states. A source being down is noted in the bundle, never fatal.
- **Triage → escalation** — every Incident is first diagnosed by `claude-haiku-4-5` in a single structured call. If its confidence falls below the threshold, the same bundle re-runs on `claude-opus-4-8`. Only the final Diagnosis is notified.
- **Incident Memory** — the bundle ends with one-liners for recent prior Incidents matching the same Target or alertname (what fired, the final Diagnosis verdict, time-to-resolve), so a recurring failure is diagnosed as a recurrence. This is where flap awareness lives: flaps create new Incidents, and memory connects them. It comes from the agent's own SQLite store, so it survives a Loki/Prometheus outage.
- **Agentic escalation** — the escalation call gets a bounded loop of read-only tools (`query_loki`, `query_prometheus`, `list_containers`, `inspect_container`, `get_incidents`) to pull evidence beyond the fixed bundle: other containers' logs, wider windows, ad-hoc PromQL, incident history. The loop is capped (default 5 calls); when the budget runs out the model must conclude. Triage stays a cheap single-shot call.
- **Read-only by design** — Docker is reached exclusively through a GET-only [socket proxy](docs/adr/0001-docker-socket-proxy.md); there is no auto-remediation. Every tool in the registry is a read.

## Usage

```bash
# On-demand diagnosis of a container, printed to the terminal
sre-agent diagnose <container>

# Webhook server for Alertmanager (listens on :8080, POST /webhook)
sre-agent serve
```

Manual diagnoses are stored in the incident history but never notify — you're already looking at the output.

## Configuration

Everything is env vars. Only `ANTHROPIC_API_KEY` is required; the defaults match the compose deployment.

| Variable | Default | Purpose |
|---|---|---|
| `ANTHROPIC_API_KEY` | — (required) | Claude API key |
| `SRE_LOKI_URL` | `http://loki:3100` | Loki base URL |
| `SRE_LOKI_CONTAINER_LABEL` | `container_name` | Loki stream label holding container names |
| `SRE_PROMETHEUS_URL` | `http://prometheus:9090` | Prometheus base URL |
| `SRE_DOCKER_PROXY_URL` | `http://docker-proxy:2375` | GET-only docker socket proxy |
| `SRE_NTFY_URL` | `https://ntfy.sh` | ntfy server |
| `SRE_NTFY_TOPIC` | — | ntfy topic to publish to |
| `SRE_TRIAGE_MODEL` | `claude-haiku-4-5` | Triage model |
| `SRE_ESCALATION_MODEL` | `claude-opus-4-8` | Escalation model |
| `SRE_CONFIDENCE_THRESHOLD` | `0.7` | Escalate below this triage confidence |
| `SRE_LOG_BYTE_BUDGET` | `40960` | Max bytes of logs in the Context Bundle |
| `SRE_MEMORY_WINDOW_DAYS` | `30` | Incident Memory lookback window |
| `SRE_MEMORY_MAX_ENTRIES` | `5` | Max prior Incidents in the bundle (`0` disables memory) |
| `SRE_TOOL_BUDGET` | `5` | Max tool calls per Escalation (`0` disables tools) |
| `SRE_LISTEN_ADDR` | `:8080` | Webhook listen address |
| `SRE_DB_PATH` | `incidents.db` | SQLite incident store |
| `SRE_ANTHROPIC_URL` | `https://api.anthropic.com` | Claude API base URL (tests point this at fakes) |

## Build, test, deploy

```bash
go build ./...   # single static binary (pure-Go SQLite, no cgo)
go test ./...    # every dependency faked at the HTTP seam — no API key or network needed
```

Deployment is one Docker image with both subcommands — see [`docker-compose.example.yml`](docker-compose.example.yml) and [`docs/deployment.md`](docs/deployment.md) for the live environment contract (Alertmanager already points at `sre-agent:8080/webhook`). The deployment doc also covers smoke-testing the CLI against the live stack from a dev machine.

## Project docs

- [`CONTEXT.md`](CONTEXT.md) — domain glossary (Incident, Target, Context Bundle, Diagnosis, Triage/Escalation)
- [`docs/design.md`](docs/design.md) — full decision record for all phases
- [`docs/adr/`](docs/adr/) — architectural decision records
- [`implementation-notes.md`](implementation-notes.md) — running log, including deviations from the PRD

## Roadmap

Phases 1–4 (CLI, webhook server, Incident Memory, agentic tool use) are implemented. Still ahead, per [`docs/design.md`](docs/design.md):

5. **MCP server** — chat about homelab status from Claude over the tailnet

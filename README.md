# homelab-sre-agent

When something breaks in the homelab, Alertmanager tells you *that* it broke. This agent tells you *why*: a single Go binary that receives Alertmanager webhooks, gathers logs, metrics, and container state around the incident, asks Claude what most likely went wrong, and pushes a plain-language diagnosis to your phone before you've opened Grafana.

It assumes a docker-compose (or Unraid) homelab that already runs Prometheus, Loki, and Alertmanager, and it needs an Anthropic API key. Cost-wise, each incident is one cheap Haiku call (a fraction of a cent), and only low-confidence cases escalate to Opus with a capped tool budget, typically a few cents. A quiet homelab costs almost nothing to watch.

Everything the agent does to your homelab is a read. Docker access goes through a GET-only [socket proxy](docs/adr/0001-docker-socket-proxy.md), there is no auto-remediation, and the optional MCP server exposes the same read-only tools and nothing else.

```
Alertmanager ──webhook──▶ sre-agent ──▶ gather Context Bundle ──▶ Claude triage (Haiku)
                                          │  Loki logs ±15m          │ low confidence?
                                          │  Prometheus panel        ▼
                                          │  Docker states        escalate (Opus + tools)
                                          ▼                          │
                                       SQLite ◀── Incident ──▶ ntfy ─┘
```

## Quickstart

**Docker / compose.** There's a multi-arch image (amd64 + arm64) on GHCR:

```bash
docker pull ghcr.io/jonaseriksson84/homelab-sre-agent:latest
```

Copy [`docker-compose.example.yml`](docker-compose.example.yml), set `ANTHROPIC_API_KEY` and an ntfy topic, attach it to your monitoring network, and add an Alertmanager webhook receiver pointing at `http://sre-agent:8080/webhook`. The [setup guide](docs/setup.md) walks through the whole path, including the Alertmanager config to copy-paste.

**Unraid.** A Community Applications template lives at [`unraid/sre-agent.xml`](unraid/sre-agent.xml); every config variable is a form field and the API key is password-masked. Until the CA store listing lands, add the raw template URL under Docker → Add Container.

## How does this compare to X?

| Project | What it is | Pick it over this when |
|---|---|---|
| [HolmesGPT](https://github.com/robusta-dev/holmesgpt) | CNCF-sandbox AI root-cause agent with a similar investigate-with-tools loop | You run Kubernetes. It's k8s-first and much bigger; this agent is "HolmesGPT for people who run docker-compose, not Kubernetes" |
| alert-explainer-style tools | Single-shot LLM explanations of an alert payload | You only want the alert text paraphrased — no log/metric gathering, no incident history |
| Versus Incident, Akmatori | Team incident-management platforms with AI features | You want on-call rotations, escalation policies, a UI — team ops, not a homelab |
| Unraid management/MCP agents | MCP or chat access to homelab state, including *write* control | You want an agent that can act on containers. This one deliberately can't |

Nothing else I found does alert-driven LLM diagnosis for a compose-based homelab, remembers past incidents, and delivers the result as a push notification, while staying read-only end to end. That gap is why this exists.

## How it works

- **Incident lifecycle.** The first firing webhook for an Alertmanager group creates an Incident, runs one diagnosis, and sends one notification. Repeat firings only bump `last_seen`, with no extra Claude calls and no extra pings. The resolved webhook closes the Incident with a low-priority "resolved after Xm" ping. A group that flaps becomes a new Incident.
- **Targeting.** The alert's `container` label picks the diagnosis Target. Without one, alert labels are fuzzy-matched against running container names, and failing that the diagnosis runs on host-level context alone. No incident is ever dropped for want of a target.
- **Context Bundle.** Deterministic and size-bounded: the target's Loki logs (±15 min, byte-budgeted keeping the newest lines), a fixed panel of downsampled Prometheus queries, and Docker container states. A source being down is noted in the bundle rather than treated as fatal.
- **Triage, then escalation.** Every Incident is first diagnosed by `claude-haiku-4-5` in a single structured call. If its confidence falls below the threshold, the same bundle re-runs on `claude-opus-4-8`. Only the final Diagnosis is notified.
- **Incident Memory.** The bundle ends with one-liners for recent prior Incidents matching the same Target or alertname (what fired, the final Diagnosis verdict, time-to-resolve), so a recurring failure is diagnosed as a recurrence. This history comes from the agent's own SQLite store, which means it survives a Loki or Prometheus outage.
- **Agentic escalation.** The escalation call gets a bounded loop of read-only tools (`query_loki`, `query_prometheus`, `list_containers`, `inspect_container`, `get_incidents`) to pull evidence beyond the fixed bundle: other containers' logs, wider windows, ad-hoc PromQL, incident history. The loop is capped (default 5 calls), and when the budget runs out the model must conclude. Triage stays a cheap single-shot call.
- **MCP server.** `serve` can expose the same tool registry over MCP streamable HTTP on a second listener, so you can chat about homelab status from any Claude client. Escalation tool use and MCP share one implementation, so they can't diverge. There is no app-level auth ([ADR-0002](docs/adr/0002-mcp-tailnet-only.md)); bind it to a Tailscale/VPN interface only, never the LAN or the internet.

## Usage

```bash
# On-demand diagnosis of a container, printed to the terminal
sre-agent diagnose <container>

# Webhook server for Alertmanager (listens on :8080, POST /webhook)
sre-agent serve
```

Manual diagnoses are stored in the incident history but never notify, since you're already looking at the output.

## Configuration

Everything is env vars. Only `ANTHROPIC_API_KEY` is required; the defaults match the compose example.

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
| `SRE_MCP_LISTEN_ADDR` | — (disabled) | MCP server listen address; tailnet-bound only |
| `SRE_DB_PATH` | `incidents.db` | SQLite incident store |
| `SRE_ANTHROPIC_URL` | `https://api.anthropic.com` | Claude API base URL (tests point this at fakes) |

## Build, test, contribute

```bash
go build ./...   # single static binary (pure-Go SQLite, no cgo)
go test ./...    # every dependency faked at the HTTP seam; no API key or network needed
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the testing convention and [docs/publishing.md](docs/publishing.md) for how releases are cut (tag `v*` → CI publishes the GHCR image).

## Project docs

- [`docs/setup.md`](docs/setup.md) — zero-to-diagnosed-incident setup guide
- [`CONTEXT.md`](CONTEXT.md) — domain glossary (Incident, Target, Context Bundle, Diagnosis, Triage/Escalation)
- [`docs/design.md`](docs/design.md) — full decision record for all phases
- [`docs/adr/`](docs/adr/) — architectural decision records, including why the MCP server has no auth and why Claude is currently the only provider

## Roadmap

All five design phases are implemented: CLI, webhook server, Incident Memory, agentic tool use during escalation, and the MCP server. Multi-provider LLM support (OpenAI, local models) is on the list but deferred; [ADR-0003](docs/adr/0003-single-llm-provider.md) explains why.

## License

[MIT](LICENSE)

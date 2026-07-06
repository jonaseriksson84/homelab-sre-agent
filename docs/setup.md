# Setup Guide

From zero to a diagnosed incident. The agent assumes a monitoring stack you probably already run: **Prometheus** (metrics), **Loki** with promtail or another shipper (container logs), and **Alertmanager** (alerts). Without them the agent starts and does nothing.

## 1. Prerequisites

| Component | Why the agent needs it |
|---|---|
| Prometheus | Metrics panel in every diagnosis, ad-hoc PromQL during escalation |
| Loki (+ promtail) | The target container's logs around the incident |
| Alertmanager | The webhook that turns an alert into an Incident |
| GET-only Docker socket proxy | Container states, restart counts, exit codes — read-only |
| Anthropic API key | The diagnosis itself ([console.anthropic.com](https://console.anthropic.com)) |
| ntfy topic (optional) | Push notifications to your phone; treat the topic name as a secret |

On Unraid, all of the monitoring pieces are available in Community Applications. Anywhere else, any compose-based stack works.

**The socket proxy is not optional.** The agent never mounts the real Docker socket ([ADR-0001](adr/0001-docker-socket-proxy.md)): it only ever talks to a proxy that allows GET requests on `/containers`. With [tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) that means `CONTAINERS=1` and `POST=0` — see the [compose example](../docker-compose.example.yml).

## 2. Run the agent

```yaml
# docker-compose.yml (trimmed; see docker-compose.example.yml for the full file)
services:
  sre-agent:
    image: ghcr.io/jonaseriksson84/homelab-sre-agent:latest
    environment:
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
      SRE_NTFY_TOPIC: ${SRE_NTFY_TOPIC}
      SRE_LOKI_URL: http://loki:3100
      SRE_PROMETHEUS_URL: http://prometheus:9090
      SRE_DOCKER_PROXY_URL: http://docker-proxy:2375
      SRE_DB_PATH: /data/incidents.db
    volumes:
      - ./data:/data   # chown 1000:1000 — the image runs non-root
    networks: [monitoring]
```

Attach it to the same Docker network as Prometheus, Loki, and Alertmanager so the default service-name URLs resolve. On Unraid, install it from the CA template instead (`unraid/sre-agent.xml`) — every variable above is a form field.

## 3. Point Alertmanager at it

Add a receiver and route (grouping by `alertname` + `container` gives one Incident per failing container):

```yaml
# alertmanager.yml
route:
  receiver: sre-agent
  group_by: ["alertname", "container"]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h

receivers:
  - name: sre-agent
    webhook_configs:
      - url: http://sre-agent:8080/webhook
        send_resolved: true   # lets the agent close Incidents and time-to-resolve
```

Container-level alerts should set a `container` label with the Docker container name — that label is how the agent picks the diagnosis Target. A typical cAdvisor-based rule:

```yaml
# prometheus alert rule
- alert: ContainerDown
  expr: time() - container_last_seen{name!=""} > 300
  labels:
    container: "{{ $labels.name }}"
```

Alerts without a `container` label still work — the agent fuzzy-matches labels against running container names, and falls back to a host-level diagnosis.

## 4. Check the log labels

Diagnoses pull the target's logs with `{<label>="<container>"}`. The default label is `container_name` (what promtail's Docker service discovery usually exposes). If your Loki pipeline names it differently, set `SRE_LOKI_CONTAINER_LABEL` — verify with:

```bash
curl -s "http://loki:3100/loki/api/v1/labels" | jq
```

## 5. Smoke test

Fire a synthetic alert and watch for the ntfy ping:

```bash
curl -s -X POST http://alertmanager:9093/api/v2/alerts -H 'Content-Type: application/json' -d '[{
  "labels": {"alertname": "SmokeTest", "container": "grafana", "severity": "warning"},
  "annotations": {"summary": "synthetic test alert"}
}]'
```

Or bypass Alertmanager entirely and run a manual diagnosis from any machine that can reach the stack:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
export SRE_LOKI_URL=http://your-host:3100
export SRE_PROMETHEUS_URL=http://your-host:9090
export SRE_DOCKER_PROXY_URL=http://your-host:2375
export SRE_DB_PATH=/tmp/sre-agent-dev.db
go run . diagnose <container>
```

## 6. Optional: the MCP server

`serve` can expose the same read-only tools over MCP streamable HTTP so Claude clients can chat about homelab status. It has **no authentication by design** ([ADR-0002](adr/0002-mcp-tailnet-only.md)) — the network boundary is the auth. Only ever publish the port on a Tailscale (or equivalent VPN) interface:

```yaml
environment:
  SRE_MCP_LISTEN_ADDR: ":8082"
ports:
  - "100.x.y.z:8082:8082"   # your host's Tailscale IP (`tailscale ip -4`), never 0.0.0.0
```

Then from any tailnet device:

```bash
claude mcp add --transport http homelab http://100.x.y.z:8082
```

If you don't run a tailnet, leave `SRE_MCP_LISTEN_ADDR` unset (the default) and the listener never starts. Don't put it on the LAN "temporarily" — anyone who can reach the port can read your logs and metrics.

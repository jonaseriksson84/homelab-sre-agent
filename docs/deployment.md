# Deployment Environment (Tower / Unraid)

The live environment the agent deploys into, recorded 2026-07-04 when Alertmanager was set up. This is the contract `sre-agent serve` must satisfy — Alertmanager is already pointed at it.

## Deployment contract

- Container name `sre-agent`, attached to the `monitoring_default` Docker network
- HTTP listener on port **8080**, webhook path **`/webhook`**
- Receives standard Alertmanager webhook JSON (version "4")
- `send_resolved: true` is set, so resolved webhooks arrive too
- Routing: grouped by `alertname` + `container`, `group_wait 30s`, `group_interval 5m`, `repeat_interval 4h`
- The agent is deployed and consuming these webhooks (live since 2026-07-05); if it is ever down, Alertmanager logs failed notification attempts — expected and harmless

## Monitoring stack

Compose project at `/mnt/user/appdata/monitoring/` on Tower (`ssh tower` → root@tower.local); manage with `docker compose` from that directory. Configs: `alertmanager.yml`, `alerts.yml`, `prometheus.yml`, `promtail-config.yaml`, `loki-config.yaml`. Pre-change backups live in `config-backups/` named `<file>.<YYYYMMDD-HHMMSS>.bak` (timestamped so they never clobber; the newest per file is last-known-good). Runtime data dirs (`loki-data`, `loki-wal`, `loki-compactor`, `*-data`) are not config.

Endpoints on `tower.local` (also reachable over the tailnet):

| Service | Port |
|---|---|
| Prometheus | 9090 |
| Alertmanager | 9093 |
| Grafana | 3000 |
| Loki | 3100 |

## Alert rules (`alerts.yml`)

`InstanceDown`, `HostHighMemory`, `HostHighCPU`, `HostDiskLow`, `ContainerRestarting`, `ContainerDown`. `ContainerRestarting` and `ContainerDown` set a `container` label with the Docker container name — the primary key for Target resolution. (Container-name alerts require cadvisor's `name` label, which needs the `--containerd` flag on cadvisor; `HostDiskLow` requires node_exporter to see the host rootfs via `--path.rootfs=/host`.)

## Testing against the live stack

Inject test alerts with `POST tower.local:9093/api/v2/alerts` (verified working end-to-end 2026-07-04). These endpoints are also what the CLI's env vars point at for smoke-testing during development:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
export SRE_LOKI_URL=http://tower.local:3100
export SRE_PROMETHEUS_URL=http://tower.local:9090
export SRE_DOCKER_PROXY_URL=http://tower.local:2375   # docker-socket-proxy, see ADR-0001
export SRE_DB_PATH=/tmp/sre-agent-dev.db
go run . diagnose <container>
```

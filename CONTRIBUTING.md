# Contributing

## Build and test

```bash
go build ./...   # single static binary (pure-Go SQLite, no cgo)
go test ./...    # no API key, no network, no Docker needed
go vet ./...
```

CI runs exactly these on every push and PR, plus a `docker build` of the release image.

## Testing convention: one seam, at the HTTP boundary

Every external dependency — Claude, Loki, Prometheus, the Docker socket proxy, ntfy — is plain HTTP with a configurable base URL. Tests point those URLs at `httptest` fakes and assert on observable behavior: what requests went out, what notifications were sent, what rows landed in a real (temp-file) SQLite store. Nothing is mocked below that seam, and internals are not asserted on.

When adding a feature, extend the existing scenario suites (`internal/pipeline`, `internal/mcpserver`, `internal/server`) in that style rather than introducing new mocking layers. If you add a config variable, the packaging test at the repo root will fail until the Unraid template (`unraid/sre-agent.xml`) declares it too — that's intentional.

## Vocabulary and decisions

- [`CONTEXT.md`](CONTEXT.md) defines the domain terms (Incident, Target, Context Bundle, Diagnosis, Triage/Escalation). PRs should use them the way the glossary does.
- [`docs/adr/`](docs/adr/) records the hard-to-reverse decisions — notably the GET-only socket proxy (0001), the auth-less tailnet-only MCP server (0002), and single-provider Claude (0003). Please don't "fix" those without reading them first.
- [`docs/design.md`](docs/design.md) is the full design record.

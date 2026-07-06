# Publishing (maintainer notes)

## Cutting a release

1. Pick a version and tag it: `git tag v1.0.0 && git push origin v1.0.0`.
2. The `release` workflow builds the multi-arch image (amd64 + arm64) and pushes `ghcr.io/jonaseriksson84/homelab-sre-agent:<version>` and `:latest`. The version is injected via the Dockerfile's `VERSION` build arg, so the MCP handshake reports it.
3. First release only: in the GitHub package settings, make the `homelab-sre-agent` package public. GHCR packages default to private.

## Community Applications submission (manual, one-time)

The template (`unraid/sre-agent.xml`) and icon live in this repo; CA submission is a human process:

1. Verify the template installs cleanly on a real Unraid box (Docker tab → Add Container → paste the raw template URL).
2. Create a support thread in the [Unraid forums](https://forums.unraid.net/) under Docker Containers, and set its URL as the template's `<Support>` element (currently GitHub issues; CA moderation prefers a forum thread, but GitHub is accepted).
3. Follow the [CA application policies](https://forums.unraid.net/topic/87144-ca-application-policies-notes/) and post in the "Add your template repository" thread with this repo's URL. CA scans `unraid/*.xml` in template repositories; alternatively keep templates in a dedicated `unraid-templates` repo if this repo accumulates unrelated XML.
4. Moderation typically responds within days; after acceptance the app appears in CA search.

# MCP server is tailnet-only, with no application-level auth

The MCP server speaks streamable HTTP and is reachable only over the Tailscale network (bound/firewalled to the tailnet interface on the Unraid host). It deliberately has no login, token, or OAuth layer: the tailnet is the auth boundary, every client device already runs Tailscale, and the tools are read-only. Do not "fix" the missing auth — the correct change, if the trust model shifts (shared tailnet nodes), is to add a bearer token check, not to expose the server publicly.

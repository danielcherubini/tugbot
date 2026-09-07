---
status: accepted
date: 2026-09-07
superseded-by:
---

# MCP layer is always-on, zero-config, unauthenticated by default (LAN-trust posture)

When the in-process MCP layer (the `internal/mcp` package, the embedded-provider pattern over the bot's single shared discordgo session) was designed, we chose: always-on (no enabled flag, no other MCP env vars; `TUGBOT_MCP_PORT` is the only knob, default 8642), and no auth middleware by default. Any client that can reach the port can act as the bot (read/post/react via the bridge tools). Chosen because the whole point of the shared-connection design is "just connect and use whatever credentials the bot has" — adding a bearer token via env would break the zero-config matter and push the credential to the client config. Accepted risk: the endpoint is open to the LAN; the tugbot host's standing is unbound in public, so the threat surface is the LAN, and v1's tool surface (read/post/react) is low-severity.

Considered options: explicit opt-in bearer token (would keep LAN clients working tokenless but require a client-side restart on every bot restart unless the token is static — rejected as friction), 127.0.0.1-only default bind (rejected — it kills pi/Claude clients on other machines, which is the primary usage), off-by-default (rejected — contradicts "the MCP endpoint just exists when the bot is running"). Consequence: enabling auth later is a breaking change for existing client configs (a third decision would be required, hence this one is hard to quietly reverse).

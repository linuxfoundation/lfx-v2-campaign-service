---
type: "Go Package"
title: "cmd/campaign-service"
description: "The LFX V2 Campaign Service."
resource: "cmd/campaign-service"
---

# cmd/campaign-service

The LFX V2 Campaign Service. `server.go` builds the HTTP server and mounts each
wired Goa service's handlers — the health/campaign, connection, brief, and
audience servers (`buildMux`). Every service the container wires must also be
mounted here: a service constructed in the container but not mounted is
unreachable (its routes 404) even though the code compiles, which is the bug the
connection-routes change originally fixed. `debug.LogPayloads()` is intentionally not applied to any
service: payloads carry bearer tokens and (for connections) plaintext provider
credentials, so DEBUG payload logging would leak secrets. `debug.HTTP()` is
still applied, but in clue v1.2.1 it does not log headers or statuses — it only
propagates the runtime `/debug` toggle into the request context (activating
debug-level logs elsewhere); it decodes no payload.

The binary is not only the HTTP server. `bootstrap-system-account` is a SUBCOMMAND of this same
binary that installs or rotates the LF-owned system ad-account credentials (see
[internal/bootstrap](internal-bootstrap.md)) and exits — it mounts nothing and serves nothing. It
lives here rather than in its own `cmd/` because ko publishes the images this repo ships, and a
second binary would need its own publish entry; a subcommand of an already-published image is
runnable as a Kubernetes Job on day one. It reads the same `PG*` configuration as the server for
the same reason: `DATABASE_URL` is not what the chart injects, so a command composing its DSN any
other way is dead in-cluster.

See [cmd/campaign-service](../../../cmd/campaign-service).

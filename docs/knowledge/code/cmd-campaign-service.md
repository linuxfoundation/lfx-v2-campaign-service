---
type: "Go Package"
title: "cmd/campaign-service"
description: "The LFX V2 Campaign Service."
resource: "cmd/campaign-service"
---

# cmd/campaign-service

The LFX V2 Campaign Service. `server.go` builds the HTTP server and mounts each
wired Goa service's handlers — currently the health and connection servers.
Every service the container wires must also be mounted here: a service
constructed in the container but not mounted is unreachable (its routes 404)
even though the code compiles, which is the bug this change fixes for the
connection routes. `debug.LogPayloads()` is intentionally not applied to any
service: payloads carry bearer tokens and (for connections) plaintext provider
credentials, so DEBUG payload logging would leak secrets; only `debug.HTTP()`
(headers/status) is used.

See [cmd/campaign-service](../../../cmd/campaign-service).

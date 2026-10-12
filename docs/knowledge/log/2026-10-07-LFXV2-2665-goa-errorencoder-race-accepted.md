# 2026-10-07 — Record the goa ErrorEncoder race as accepted, with its upstream fix

**Note** — review of #282. With `formatter=nil` on the generated servers (required, so named error
bodies keep their generated shape), goa v3.25.3's `goahttp.ErrorEncoder` assigns its default
formatter inside the returned closure on every non-named error: an unsynchronized write of the same
value, reported by `-race` under concurrent errors on one endpoint. Verified in the v3.25.3 source;
goa v3.30.0 moved the nil check outside the closure. Recorded as an accepted known upstream issue
at the wiring in `server.go` and in the cmd concept; the remedy is a goa bump to v3.30.0 or later
(regenerating `gen/`), tracked as a follow-up. `main` carried the same race before. See
[cmd/campaign-service](../code/cmd-campaign-service.md).

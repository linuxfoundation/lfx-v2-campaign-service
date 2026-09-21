# 2026-09-11 — LFXV2-2770: nine implemented endpoints the gateway could not reach

**Fix** — the HTTPRoute regex and the Heimdall `project-api` RuleSet now carry the nine
`audience-builder` leaves. Until they did, the endpoints were mounted, generated, tested and
completely unreachable: `grep -rn audience charts/ --include=*.yaml` returned nothing.

This is the failure that had already been reported from the UI as a 404 with
`"Audience Builder not found"`. Heimdall is default-deny, so a path the RuleSet does not rule is
rejected before the FGA check ever runs, and a path the HTTPRoute regex does not select is never
forwarded at all — which presents to a caller as a 404 **from the edge**, indistinguishable from a
route that was never written. The mirror-image hazard was already documented here (the five
`{provider}/metrics` paths, routed and ruled but unimplemented); this is the other direction, and
`parity_test.go` could not catch either, because it compares the RuleSet to the regex and reads
neither `design/` nor `gen/`.

The family is enumerated leaf by leaf rather than admitted with `audience-builder/**` and a free
`(/.*)?` tail. Three reasons, in order of weight. `compose-master` creates real contact lists in a
production HubSpot portal and is not idempotent, so routing a path the service does not serve
costs more here than for a read-only family. `POST /signal-list` is specified in the plan and
deliberately unimplemented, and the bare base path serves nothing — a wildcard would authorize
both. And two leaves are two segments deep (`lists/search`, `qa/run`), so a single-segment capture
would not have covered the family anyway.

`parity_test.go` gained positive rows for all nine leaves plus negative rows for the base, the
trailing slash, `signal-list`, and a path hanging off `compose-master`. The negative rows are the
ones that matter: a future edit that relaxes either side to a free tail passes every positive row.
The witness derivation covers the rest — each new alternation leaf in the regex yields a witness
that the RuleSet must also match, so a one-sided edit fails the build.

`helm` is not installed locally, so all three chart tests skip here. Parity was verified by hand
against the rendered path lines instead: all 62 curated rows agree between the two matchers, and
every RuleSet pattern's witness matches the route regex. The test itself will run in CI.

The endpoints this routes are described in
[[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].

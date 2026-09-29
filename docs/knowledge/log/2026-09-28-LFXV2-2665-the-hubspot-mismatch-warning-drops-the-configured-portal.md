# 2026-09-28 — LFXV2-2665: the HubSpot mismatch warning drops the configured portal

**Fix** — PR #228 review, security nit raised by dealako. One log attribute removed, one guard
test added. No behaviour change beyond the log line.

`HubSpotDispatcher.ProbeConnection` logs a warning when the connection's configured `portal_id`
differs from the portal its token authenticates into. The line is not a verdict — a stale
`portal_id` describes a connection that works, since nothing routes on it — it exists so whoever
chases a dead `app.hubspot.com` deep link can find out why. It was carrying both portals.

Only the authenticated one stays. The two halves are not symmetric, which is what made the nit
worth obliging rather than declining:

- The **authenticated** portal is derived from the token inside this call and appears nowhere a
  reader can look it up. Without it the line announces that deep links are broken and withholds
  where they point, which is the entire diagnostic.
- The **configured** portal is the operator's own stored input. It is on the connection row that
  this same log line already names by `project_id`, so logging it copies operator-supplied data
  into the log stream and buys nothing the row does not already answer.

`TestHubSpotProbe_MismatchLogsOnlyTheDerivedPortal` asserts both directions — the derived portal
present, the configured one and its literal value absent. The guard is the point of the commit:
an attribute pair with one half missing reads as an oversight, and the next person to touch this
line will complete it unless something fails when they do.

The same review raised `project_id` on the probe outcome arms. That one is declined here and
tracked as issue #229: the field is the only thing identifying whose connection failed, this branch
did not introduce the convention, and it appears at 131 non-test call sites across the repo — 21
in `internal/service/connection.go` alone. Removing it in the probe arms only would leave the
repo contradicting itself about whether the field is sensitive. If it goes, it goes everywhere,
under one mechanism, in a change that is about logging rather than about LFXV2-2665. #229 carries
the four options and the cost of each.

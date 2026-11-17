# 2026-11-17 — #563 Microsoft part 3 review fixes

**Fix** — `ValidateAdGroupNameSuffixes` now also refuses a URL-shaped `adGroups[].nameSuffix`
that `redact.SnapshotText` leaves unchanged: the redactor keeps scheme + host, so an origin-only
`https://x.co` passed the "changed by the redactor" check and was persisted as a suffix despite
the no-URL rule (#623 review). The added check (`urlShapedSuffixRE`) refuses any `scheme://` and
any `www.` host. A bare host alone (`example.com`) is still accepted: it cannot be told apart from
a label such as `Node.js`, and with a path, query or credentials the redactor already changes it.

**Fix** — In `internal/dispatch/microsoft_bid.go` the `microsoftCreateSettled` doc paragraph sat
directly above `microsoftCreateSettlesAt`'s, forming one doc block on the wrong function; it now
sits on `microsoftCreateSettled` (#623 review nit).

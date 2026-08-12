# 2026-08-10 — Meta's shared credential resolver dropped system-row attribution

**Fix** — `resolveMetaCredentials` applies `systemScoped` in a `defer`, which is the right
shape: it is the reason a fourth not-usable return added later cannot forget the tag. But the
defer read the function's NAMED RETURN `res`, and every not-usable return sets `res` to nil on
its way out. `systemScoped` is a no-op on a nil receiver
(`internal/dispatch/creds.go:178-181`), so the tag was silently dropped from exactly the errors
that need it, for every caller of the resolver: create, toggle and metrics.

Nothing failed and nothing logged. The error stayed correct in every other respect — right
sentinels, right message, right status class — so the only symptom was the one the tag exists
to prevent: a project running on the LF system fallback is answered a 400 telling it to go and
edit a connection it does not own and cannot address, while the operator who installed the LF
credential is never paged.

The defer now closes over a separate `conn` binding taken once, immediately after
`creds.resolve` succeeds. The named-return `res` is left free to be nilled by the error paths.

## Why it survived until now

Discovery masked it. `resolveMetaDiscoveryClient` carried its OWN copy of the same three
stored-state checks — active status, decodable blob, non-empty access token — over a plain
local rather than a named return, so `ListAccounts` tagged correctly the whole time. The
Meta half of the system-attribution invariant was pinned only through that path
(`TestMeta_ListAccounts_AttributesSystemRowDefects`), so the suite agreed the invariant held
while three of its four callers violated it.

Collapsing the duplicate onto the shared resolver is what surfaced this, and that is the
argument for the collapse rather than an accident of it: with two copies classifying the same
three conditions, one path's correctness said nothing about the other's, and any later change
to either — a fourth check, a different sentinel, a message that stops dropping the decode
cause — would have applied to only one of "can this connection dispatch?" and "can this
connection be asked what it reaches?".

## Regression guard

`TestMeta_SystemScopedCoversEveryCallerOfResolveMetaCredentials` runs all three defect classes
against all FOUR callers, asserting `ErrSystemConnectionNotUsable` on the system-scoped row and
its absence on the project's own row. Discovery is included even though it was never broken, so
the path that was already right cannot regress while attention is on the three that were not.

Verified by mutation: restoring the defer to read the named return fails the test on all three
defect classes across every caller — including discovery, which now shares the resolver.

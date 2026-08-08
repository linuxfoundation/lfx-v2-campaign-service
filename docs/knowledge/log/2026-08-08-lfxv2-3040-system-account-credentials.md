# 2026-08-08 — The system account is a connection row, and only an absence falls back to it

**Update** — a project that has connected no ad account of its own now dispatches through
the LF-owned system account. It is an ordinary connection row at the reserved project
scope `model.SystemProjectID` (`system:linuxfoundation`), and `credsSource.resolve`
falls back to it when the project's own lookup misses.

## The first attempt shipped nothing, and passed every gate doing it

The earlier cut of this gap was an `LFX_SYS_*` environment block: package-level maps, a
mutex, a `SetupSystemCredentials` wired into the container, chart values, and a
`loadSystemCreds` that returned `nil, nil` unconditionally with a TODO. It compiled, it
vetted, it linted, and every path it added was unreachable — the fallback could never
fire, because the maps were never populated. **A framework with no implementation behind
it is not a smaller version of the feature; it is a larger version of nothing**, and it
is harder to see because the diff looks like progress.

Discarded entirely rather than completed. A system account needs exactly what a project
account needs — encryption at rest, an account id, provider config, a status, a version
for `If-Match`, an `updated_by` audit trail. All of it already exists on the connection
row. Choosing a reserved scope meant the credentials-first bootstrap flow and the
account-discovery endpoint work on the system account with no code of their own, and the
whole change came to a fallback branch plus a guard.

## Only a genuine absence falls back

The safety of the fallback rests on one asymmetry: `ErrNotFound` at the project scope
falls back; nothing else does. A repo error, an empty credential blob, a decrypt
failure — each means the project HAS a connection that needs attention, and quietly
running its campaign on the Linux Foundation's own ad account instead would spend LF
money on a request the project believed was billed to itself. A missing row is the one
state where no such intent was ever recorded.

A failure of the FALLBACK lookup is likewise not an absence. Reported as one it answers
"you have no connection" — a 404 — when the truth is that the database did not answer, a
503. This is the false-absence class in a new place: the cost of a wrong absence here is
not a duplicate campaign but a campaign billed to the wrong party.

Both dispositions are pinned by tests, using a fake keyed by project scope. The obvious
fake — the existing `fakeConnReader`, which returns the same row for every project —
cannot tell the two scopes apart, so a test built on it would pass against an
implementation that never consults the system scope at all.

## An unreachable value is not an unreachable row

`projectSlugProblem` enforces `^[a-z0-9]+(-[a-z0-9]+)*$`, which the colon cannot satisfy,
so no create endpoint can plant a row at the reserved scope. That reasoning is correct
and it is not enough: `get`/`update`/`delete`/`test`/`set-credential` are deliberately
permissive on `project_id` to keep historical UUID-keyed rows reachable, so an existing
system row would have been readable, rewritable and deletable by anyone who could reach
the connections API for any project at all.

**"It cannot be created" and "it cannot be modified" are different guarantees, and a
validator that only runs on create supplies the first.** `rejectSystemScope` supplies the
second, at the six shared helpers in `internal/service/connection_handler.go` — one choke
point rather than forty-odd per-provider adapters. It answers 404, not 403: confirming
that something is there is itself a disclosure.

The service test installs the system row in the repo before each call. Against an empty
store the repo answers "not found" on its own, so the assertion would pass against a
service with no guard whatsoever.

LFXV2-3040

## A choke point only covers what passes through it

The guard went in at the shared helpers in `internal/service/connection_handler.go`
because all six connection endpoints reach storage through them — one place instead of
forty-odd per-provider adapters. That reasoning was right and the conclusion was wrong:
there is a seventh endpoint taking a caller-supplied `project_id`, and it is the one that
does not use those helpers. `ListGoogleAdsAccounts` calls `orch.ReadAccounts` directly, so
a `GET` on the reserved scope decrypted the LF credential and enumerated the Linux
Foundation's own ad accounts — the exact disclosure the other six answer 404 to avoid.

The tell was in the PR's own prose: it said "all six", and six was derived from the
abstraction rather than counted from the routes. When a guard is justified by "every path
goes through here", the claim to verify is not the guard — it is the *every*. The test now
hands the discovery case a working orchestrator, because without one the call 503s before
the guard is reached and would pass against an unguarded implementation.

## An installer is part of the feature, not a deployment detail

The reserved scope is unaddressable over HTTP, which is the point — and it means no
request can install the credentials either. The first version of this change shipped with
no way to put a row there at all. That is twice now that this gap produced something which
compiled, vetted, linted and did nothing: the first attempt could not read the system
credentials, and this one could not write them.

`sysacct-bootstrap` speaks to the repository and the encryptor directly, so its row is
indistinguishable from an API-written one. Two properties came straight from bugs the
tests then pinned: `SetCredential` bumps the version, so an `Update` gated on the version
read *before* it leaves a rotation half-applied; and only `ErrNotFound` may create,
because creating on top of an existing-but-unreadable row overwrites a credential nobody
meant to replace. That second one is the fallback's own absence-versus-uncertainty
asymmetry arriving somewhere else, which is a decent sign it is the real invariant.

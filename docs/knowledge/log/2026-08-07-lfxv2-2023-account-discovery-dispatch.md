# 2026-08-07 — The dispatch adapter for account discovery, and the cold-start it has to survive

**Update** — New capability on PR #84 (`internal/dispatch/googleads.go`, `creds.go`,
`internal/domain/errors.go`, `internal/domain/model/connection.go`, and their tests).

The `ReadAccounts` adapter is a thin translation from the Google Ads client's
`AccessibleCustomer` rows to the domain's account shape, with one thing that is not thin: the
credential resolution in front of it.

**Discovery runs before an account exists.** Every other Google Ads dispatch path resolves a
connection that already carries a chosen `CustomerID`; this one is the call that produces the
list the operator chooses FROM, so requiring a stored account id would be a chicken-and-egg —
the endpoint could only ever be reached by someone who no longer needs it. `resolveGoogleAdsClient`'s
account precondition is therefore relaxed for this path only, and the platform client's
`doRequestValidated` is what makes that safe rather than merely permissive: the id is not
skipped, it is not required by a call that never interpolates it.

An unusable connection is still rejected. Relaxing "an account has been chosen" is not the same
as relaxing "the credential is present and well-formed", and the tests pin the difference —
discovery works before an account is chosen, and still fails on a connection with no usable
credential.

The empty case is a list, not an absence. An account list that comes back empty means the
credential reaches nothing, which is a legitimate answer the caller must be able to render;
returning nil would make it indistinguishable from a failure at the layer above. The adapter
returns an empty non-nil slice, and a test pins it, because this is the kind of thing that is
correct on the day it is written and silently regresses through a refactor.

This PR is the dispatch adapter. It sits on the platform client
(`feat/LFXV2-2023-accounts-platform`); the HTTP endpoint and its service wiring land next.

## Review pass — what the adapter actually promises on its own

Three findings on PR #84, and they share one root: the adapter was documented as if the
service layer that consumes it already existed.

**The docs described a contract that is not in this tree.** `internal-dispatch.md` and the
`ListAccounts` godoc both described an `AccountLister` interface, an
`Orchestrator.ReadAccounts` that type-asserts it, `ErrAccountsUnsupported`, and a 400/503
status mapping. `internal/service/orchestrator.go` in this branch declares only
`StatusToggler` and `MetricsReader` — none of that exists until the endpoint PR. A doc that
describes the intended end state as if it were present is worse than one that says nothing:
the next reader greps for `AccountLister`, finds nothing, and cannot tell whether the doc is
aspirational or the code regressed. Both are now scoped to what this PR contains, and say
explicitly that the orchestration lands next.

**The one promise that IS this PR's to keep** is that `domain.ErrNotFound` survives credential
resolution. That is what lets the caller answer 404 ("no connection configured — go create
one") rather than 503 ("the provider is down — retry"), and the two ask the operator to do
opposite things. `resolve` wraps with `%w` for exactly this; nothing pinned it, so flattening
the wrap to a plain string error would have collapsed 404 into 503 with every test still
green. `TestGoogleAds_ListAccounts_MissingConnectionKeepsErrNotFound` now pins it, and also
asserts Google is never contacted — a missing connection has no credential to send, so
reaching upstream would mean the guard ran too late.

**Whitespace-only credentials passed as present.** `!= ""` accepts `"   "`, which is what a
copy-paste into a credential form routinely produces. The value then reached Google and
failed there as an opaque upstream error, instead of locally as the incomplete-credentials
error that names the field to fix. The trim is now applied IN PLACE, before the check, so the
strings tested for emptiness are the same strings handed to `NewClient` — trimming only
inside the check would have been strictly worse, passing the guard and still sending the
padded value. The regression test covers both halves.

Also added the missing translation test: `AccessibleAccount.Label` comes from the
`descriptiveName` that only appears on manager-hierarchy-expanded rows, and no existing test
exercised a populated one — dropping the field would have left every discovery test passing.

All three revert-verified.

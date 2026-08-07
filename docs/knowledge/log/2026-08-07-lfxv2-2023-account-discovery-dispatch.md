# 2026-08-07 — The dispatch adapter for account discovery, and the cold-start it has to survive

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

---
type: "Code Concept"
title: "internal/bootstrap"
description: "Installs and rotates the LF-owned system ad-account credentials that projects with no connection of their own fall back to."
resource: "internal/bootstrap"
---

# internal/bootstrap

Installs and rotates the connection row for `model.SystemProjectID` — the LF-owned ad accounts a
project with NO connection of its own falls back to. The package exists because that scope is
deliberately unreachable over HTTP: `rejectSystemScope` refuses it on every connection route, so
without an out-of-band installer the fallback ships permanently empty and the feature is off. It is
driven by the `bootstrap-system-account` subcommand of the service binary (see
[cmd/campaign-service](cmd-campaign-service.md)), not by a separate image.

It writes through `domain.ConnectionRepository` and `domain.Encryptor` — the same two ports the
HTTP layer uses — so the row it produces is indistinguishable from an API-written one and needs no
special-casing anywhere downstream.

## Writing past the API means inheriting its validation

This is the package's central hazard. Every check the connection handlers perform sits in front of
the API, and this installer goes around it, so anything not re-implemented here is simply not
enforced on the system row:

- **Required credential keys** (`requiredCredentialKeys`) mirror the `Required()` lists in
  `design/connection.go`, in the snake_case WIRE form. Not the Go struct field names: the stored
  blob and the dispatch structs are both untagged, so `encoding/json` falls back to a
  case-insensitive match that bridges `clientId` but not `client_id`. `credentialKey` folds both
  spellings to one, and two spellings of the same field in one document are refused rather than
  resolved by map iteration order.
- **Values are decoded as STRINGS**, not merely checked for presence. Every dispatcher unmarshals
  these into string members, so `"client_id": 123` or `"  "` would install cleanly, exit 0, and
  fail at dispatch.
- **Surrounding whitespace is REFUSED, not trimmed.** Testing `TrimSpace(v) == ""` proves a value
  is not blank and says nothing about one that merely has padding — and the ORIGINAL `RawMessage`,
  padding included, is what gets encrypted. `"access_token":" token "` therefore installed cleanly
  while LinkedIn's preflight (`internal/platform/linkedin/client.go`) refuses a padded token, so
  the row every unconnected project falls back to was one every dispatch rejects. Refused rather
  than canonicalized: a credential is opaque here, and silently rewriting one would hide a
  truncated paste. Padding INSIDE a value, and padding on a key the provider does not require, are
  left alone — a secret's interior is not this command's business.
- **Shape rules** (`valueShapes`) come from TWO sources, because that is where they live:
  `design/connection.go` `Pattern()` for LinkedIn, Meta and X, and the runtime validators for
  Google Ads, Microsoft and Reddit, whose designs check presence alone. Mirroring only the design
  let `-provider google-ads -account-id foo` install and poison the shared fallback.
- **Required non-secret config** (`requiredConfigKeys`) is checked against the map about to be
  WRITTEN, not the flags as typed, so a key already on the row satisfies a rotation.

## Preserve, set, remove — an omission means different things at different times

`accountID` and `providerConfig` are tri-state, and which state an omission stands for depends on
whether the row already exists. On a FIRST install an omitted `-account-id` is the credentials-first
state, legal only where the account can be chosen afterwards (`accountDiscoveryProviders`, Google
Ads and — since LFXV2-3061 — Meta). Membership is narrower than "the dispatcher can discover
accounts": the other half of a completable lifecycle is that the path needing an account id fails
in a way that NAMES the missing choice. Meta is the one provider where the halves ever came
apart — it gained a discovery endpoint in LFXV2-3062 and was still excluded, because its
`Dispatch` returned a generic error for an empty id. LFXV2-3061 added the tagging
(`requireMetaAccountID` → `domain.ErrAccountNotSelected`, reported by `unusableConnectionReason`
as `account_not_selected`) and Meta joined the map. That token reaches an operator through the
dispatch-failure LOG LINE rather than the polled job result, because `dispatchPlatform` collapses
every dispatcher error into `"platform campaign creation failed"`; Meta's toggle and metrics need
no account id, so create — the asynchronous path — is its only account-needing one. Of LinkedIn, Microsoft, Reddit and X, only Microsoft has BOTH halves, as of LFXV2-3064: Reddit and
X still lack discovery, and LinkedIn — which gained a discovery endpoint in that same ticket — is
the one provider missing the OTHER half. `resolveLinkedInCredentials` does tag a missing account
with `domain.ErrAccountNotSelected`, but `LinkedInDispatcher.Dispatch` does not call it; the create
path resolves inline and returns a bare `notCreated`, so the missing choice is never named.
Microsoft, Reddit and X all tag it on a path create actually reaches. Either way an account-less row for them stays a dead row and the map
keeps them out — see the comment on the map itself for the full reasoning. On a ROTATION the same omission means KEEP, because a rotation should not have to
restate the whole row.

Preserve-by-default cannot express a removal, and this scope has no other writer that could —
`rejectSystemScope` blocks HTTP — so without an explicit clear an optional column became permanent.
`login_customer_id` is the real case: it names the manager account requests are issued through, and
when that path changes the old value is not merely stale, it is sent as a header on every dispatch.
`-clear-account-id` and `-config key=` (an empty value) are the removals; a clear is skipped by
`requireShapes` (an instruction has no shape to match), lands in the value `requireConfig` and
`requireAccountID` already check — so clearing a REQUIRED column is refused by the rules that were
already there — and is refused outright before the row exists, where obeying it and ignoring it would
produce the same row and success would be reported for an instruction that never ran.

## Rotation is idempotent, and it is ONE version-gated write

A second run rotates onto the existing row rather than failing the singleton constraint, which is
what makes the command safe in a deployment job. `mergeConfig` overlays supplied flags on the
existing config because `Update` rewrites every column — replacing would NULL siblings a flag did
not mention.

Account, config and credential go in one `UpdateWithCredential` gated on the row's version, which
is what makes a partial rotation unreachable. `Update`-then-`SetCredential` was two writes and the
order only chose WHICH mixed state a crash left behind — the new account with the old credential,
or the reverse — and neither is a state dispatch should ever observe. Concurrency was the sharper
half: `SetCredential` is not version-gated, so two simultaneous rotations could commit one run's
account beside the other's credential with nothing detecting it. Now the second writer loses the
version check and the command says nothing was written and to rerun, which is true and actionable.

See [internal/bootstrap](../../../internal/bootstrap).

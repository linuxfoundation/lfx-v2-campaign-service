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
- **Shape rules** (`valueShapes`) come from TWO sources, because that is where they live:
  `design/connection.go` `Pattern()` for LinkedIn, Meta and X, and the runtime validators for
  Google Ads, Microsoft and Reddit, whose designs check presence alone. Mirroring only the design
  let `-provider google-ads -account-id foo` install and poison the shared fallback.
- **Required non-secret config** (`requiredConfigKeys`) is checked against the map about to be
  WRITTEN, not the flags as typed, so a key already on the row satisfies a rotation.

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

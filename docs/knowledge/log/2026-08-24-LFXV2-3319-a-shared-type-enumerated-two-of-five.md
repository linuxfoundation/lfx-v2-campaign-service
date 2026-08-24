# 2026-08-24 — a shared type's description enumerated two of five providers

**Fix** — `design/connection.go`: the `AccessibleAccount` `id` attribute documented only two
id formats —

> Google Ads: bare digits (8666746580). Meta: act_-prefixed (act_8666746580).

— while **five** discovery methods already reuse the type. `list-linkedin-ads-accounts`,
`list-microsoft-ads-accounts` and `list-twitter-ads-accounts` all return
`ArrayOf(AccessibleAccount)`, so LinkedIn, Microsoft Ads and X/Twitter callers read an `id`
contract that did not describe their id.

Not only X was missing. The thread named X; the class was three providers.

## The forms, taken from the code that ENFORCES them

Not from the design examples, which are the thing under suspicion:

| Provider | Form | Enforced at |
|---|---|---|
| Google Ads | bare digits | design example `8666746580` |
| Meta | `act_`-prefixed | design example `act_8666746580` |
| LinkedIn | bare digits | `internal/platform/linkedin/targeting.go:19` `accountIDRE` (digit-only) |
| Microsoft Ads | digits only | `internal/platform/microsoft/client.go:153`, refused at `:658` |
| X/Twitter | alphanumeric handle, no prefix | `internal/platform/twitter/client.go:1483` `accountIDRe` = `^[A-Za-z0-9]+$` |

X's regex comment names `18ce54d4x5t` as the real shape, which matches the method's own
example — so the method-level examples were right all along. Only the shared type's
description was stale.

## Why this is the SAME finding as the roster fragment, in a surface its pin cannot reach

[2026-08-22](2026-08-22-LFXV2-3319-pin-the-roster-stop-correcting-it.md) established the rule
for this repo: **an enumeration of members goes stale silently, so pin what a compiler can see
and stop restating what it cannot.** `TestAccountListerProseMatchesTheInterface` pins the
roster in the HTTPRoute regex, the Heimdall RuleSet and `ruleset.md`.

It does not reach a Goa attribute description, and no test can: the description is prose inside
a design DSL, asserted by nothing, and it drifts the moment a provider is added. This
enumeration had already survived three provider additions without anyone noticing, which is
precisely the failure mode that fragment predicted.

So the fix follows that fragment's own remedy rather than adding a fourth correction: the
description now states the **shape** — an opaque, per-provider, store-verbatim string — and
names the forms as illustration rather than as a closed contract. A shape statement stays true
as providers are added. The per-provider FORM stays in each method's result-level example,
which is the one place a new method cannot omit without the author noticing, because Goa
fabricates a lorem-ipsum example when it is missing.

## A preserved clause is an adopted claim

The type's preamble comment ended:

> The description on `id` carries both formats, which is where the contract belongs.

"Both formats" was itself the stale enumeration, one layer up — it encoded the two-provider
world in a sentence describing where the contract lives. Fixing the attribute and leaving that
line would have left the file asserting a two-provider contract in its own explanation of
itself. It now describes the split (shape in the type, form in each method) rather than
counting the providers.

## Regeneration

A `Description` change is an API-surface change: `make apigen` regenerates the OpenAPI, and the
four `cmd/campaign-service/kodata/gen/http/openapi*` copies — what the deployed pod serves —
were confirmed byte-identical to `gen/http/openapi*` with `cmp`. The generated diff is
comment-only across `service.go` and the client/server `types.go`.

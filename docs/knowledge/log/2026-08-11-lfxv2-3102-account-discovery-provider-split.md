# 2026-08-11 — Account discovery reads by provider, not as one narrative

**Docs** — `docs/knowledge/code/internal-dispatch.md` (LFXV2-3102). Splits the account-discovery
section under `### Google Ads`, `### Meta`, and `### Shared: credential resolution and decrypt
classification`. The change is almost entirely movement — every paragraph goes under the heading
that owns it — with exactly one sentence rewritten: the `ListAccessibleCustomers` paragraph opened
"Google Ads is the only implementation today", and the heading it now sits under says that better
than the sentence did. That exception is spelled out below rather than rounded off, because "no
prose was rewritten" is the kind of summary a later reader would trust instead of the diff.

A second sentence was rewritten in review. The `ListAccessibleCustomers` paragraph asserted the
call goes to `customers:listAccessibleCustomers` flatly, but `Client.ListAccessibleCustomers`
branches on mode first and returns through `expandManagerHierarchy` whenever `login_customer_id`
is set (`internal/platform/googleads/client.go:1025-1027`) — the flat endpoint is never reached in
manager mode. Under a `### Google Ads` heading that unqualified claim reads as the provider's only
path, so it now names both modes. Documenting one execution path as if it were the whole is the
same defect this split was meant to remove, one level down.

## What the flat section had become

The section was written when Google Ads was the only implementation, so provider-specific detail
and cross-provider contract sat in one sequence with nothing marking which was which. Meta's
discovery (LFXV2-3062) landed in the MIDDLE of that sequence — `### Meta` opened at line 402 of a
section running 325–560, with the manager-id, `ListAccessibleCustomers` and bootstrap-lifecycle
paragraphs all still below it. That placement is the whole problem: the result read as though the
Google Ads
lifecycle rules — `Required("account_id")` dropped, `ErrAccountNotSelected` on the paths that
need an id, the `active`-with-empty-`account_id` intermediate state — described both. They do
not. The document says so explicitly in one parenthetical, which is exactly the wrong weight for
a fact a reader has to carry through forty lines of Google Ads specifics.

Two placements were actively misleading rather than merely unstructured:

- The **manager-id duplication** paragraph (`storedCustomerIDRE` vs
  `Client.validateLoginCustomerID`) sat *after* the Meta material, so the nearest preceding
  context for a Google-Ads-only invariant was a different provider.
- The **`ListAccessibleCustomers`** paragraph opened "Google Ads is the only implementation
  today", which stopped being true when Meta landed. Under a `### Google Ads` heading the
  sentence is unnecessary rather than wrong, so it is gone.

## Why this is a documentation change and not a code one

Nothing in `internal/dispatch` moved. The risk this closes is a reader applying a Google Ads
lifecycle rule to Meta and concluding a credentials-first Meta connection is supported — which,
**as of `main` at the time of writing**, it is not: Meta's campaign create does not yet fail with
`account_not_selected`. That conclusion would be drawn from correct sentences read in the wrong
scope, which no test can catch and no code change can prevent.

That "as of `main`" is load-bearing, not hedging. LFXV2-3061 (PR #116) adds exactly the missing
tagging and makes credentials-first Meta connections supported, so this sentence has a known
expiry. Both branches edit the same `Required("account_id")` parenthetical in
`internal-dispatch.md`, so whichever merges second gets a real conflict rather than a silent
staleness — the resolution must keep #116's wording, since after that merge the fact this
paragraph turns on is no longer true.

## Verification

`go run ./cmd/okfvalidate ./docs/knowledge` clean. Every claim in the moved text was checked
against the code it describes at its new heading; the split is the whole change.

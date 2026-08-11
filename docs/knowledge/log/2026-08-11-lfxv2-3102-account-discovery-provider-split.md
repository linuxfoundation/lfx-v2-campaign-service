# 2026-08-11 — Account discovery reads by provider, not as one narrative

**Docs** — `docs/knowledge/code/internal-dispatch.md` (LFXV2-3102). Splits the account-discovery
section under `### Google Ads`, `### Meta`, and `### Shared: credential resolution and decrypt
classification`. No prose was rewritten; every paragraph moved under the heading that owns it.

## What the flat section had become

The section was written when Google Ads was the only implementation, so provider-specific detail
and cross-provider contract sat in one sequence with nothing marking which was which. Meta's
discovery (LFXV2-3062) was appended to the end, and the result read as though the Google Ads
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
lifecycle rule to Meta and concluding a credentials-first Meta connection is supported — it is
not, because Meta's campaign create does not yet fail with `account_not_selected`. That
conclusion would be drawn from correct sentences read in the wrong scope, which no test can
catch and no code change can prevent.

## Verification

`go run ./cmd/okfvalidate ./docs/knowledge` clean. Every claim in the moved text was checked
against the code it describes at its new heading; the split is the whole change.

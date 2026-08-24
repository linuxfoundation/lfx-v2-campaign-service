# 2026-08-24 — LFXV2-3319 a docs conflict where both sides had new content

**Update** — merging `origin/main` conflicted in five files: four generated OpenAPI specs
(two in `gen/http`, two in their `cmd/campaign-service/kodata` copies) and one hand-written
doc, `docs/api-catalog.md`. The generated four were resolved by clearing the index and
re-deriving with `make apigen`, which is the only resolution that can be right for a file
that is a function of the design.

The doc conflict is the one worth recording, because BOTH sides had added content to the
SAME table row and either side taken whole silently deletes the other's.

This branch had widened the account-discovery row from four `AccountLister` providers to
five, adding `twitter-ads`. Main had, in the same row, rewritten the **500** clause to add
a third case — a defect in this service (`ErrServiceDefect`, `reason=token_request_rejected`)
matched ABOVE the 400 arm, because that 400 tells the reader to go and check stored fields
that are all correct in this case. Main's own added sentence names the trap directly:
"Stated as the property rather than a count: the previous wording fixed a number that the
next sentinel routed here falsified."

So `--ours` on that file loses the new 500 case, and `--theirs` loses X/Twitter from the
provider list — and neither loss breaks a build or a test, because it is prose. The row was
rebuilt as the union: this branch's five-provider list and X/Twitter narrative, with main's
three-case 500 clause substituted in verbatim.

`--ours` was in any case unusable here as a per-hunk tool: it resolves a WHOLE FILE, so
using it for this one row would have reverted every other hunk the merge had already
settled in the same document.

**Verification** — the merged catalog is 942 lines against ours 940 and main 942, and the
HTTP-verb/path row set is an exact union at 38 rows with nothing missing and nothing
invented. The regenerated `openapi3.json` likewise unions to 42 paths (ours 42, main 41),
carrying both main's `MDP-approved` LinkedIn refresh text and this branch's
`connection-twitter-ads/accounts` route. All four kodata specs compare byte-identical to
their `gen/http` originals after `apigen`.

The cursor-walk fix this branch carries was untouched by the merge — `git diff` reports no
change under `internal/platform/twitter/`. Mutating `cursorVerdict`'s empty-string arm back
to `cursorExhausted` compiles and fails three tests across BOTH walks at once —
`TestListAdAccounts_EmptyCursorIsNotExhaustion`,
`TestFindByNameFullPageWithEmptyCursorIsError` and
`TestFindLineItemByNameFullPageWithEmptyCursorIsError` — which is the property the shared
helper exists to guarantee: the two walks can no longer drift apart, so a full page
carrying `"next_cursor":""` cannot again be read as a genuine not-found and drive a
duplicate campaign create.

# 2026-08-11 — Account discovery reads by provider, not as one narrative

**Docs** — `docs/knowledge/code/internal-dispatch.md` (LFXV2-3102). Splits the account-discovery
section by provider. The rendered heading sequence is now Google Ads → Meta → Google Ads → Meta →
Google Ads → `### Shared: credential resolution and decrypt classification`, because the material
was written as one narrative when Google Ads was the only implementation and each later provider
was appended wherever it fit rather than at a boundary.

**The change is mostly movement, but not only movement, and the count grew across review.** It
opened claiming "exactly one sentence rewritten"; by the end there were four rewrites plus new
narrative at the reopened headings. Each is recorded below in the round that produced it. The
running summary is corrected here rather than left standing, because a first paragraph asserting
"almost entirely movement" is exactly what a later reader trusts instead of reading the diff —
which is the same failure mode this entry keeps documenting one level down.

The first rewrite: the `ListAccessibleCustomers` paragraph opened "Google Ads is the only
implementation today", and the heading it now sits under says that better than the sentence did.

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
lifecycle rule to Meta — a conclusion drawn from correct sentences read in the wrong scope, which
no test can catch and no code change can prevent.

**The example that motivated this has since resolved itself, and that is worth recording.** When
this split was written, a credentials-first Meta connection was NOT supported: Meta's campaign
create did not fail with `account_not_selected`, so a reader who applied the Google Ads bootstrap
rule to Meta would have concluded something false. LFXV2-3061 (PR #116) has since merged and
added exactly that tagging, so credentials-first Meta connections are now supported and both
providers really do share the lifecycle.

The heading split is not made pointless by that. What it prevents is the *inference* — a reader
being unable to tell which provider a paragraph governs — and that failure mode is independent of
whether any particular inference happens to be true this week. The two branches did edit the same
`Required("account_id")` parenthetical, and the conflict on merge was resolved by keeping #116's
wording, which is why the concept file now reads "Both lifecycles are reachable as of
LFXV2-3061".

## Verification

`go run ./cmd/okfvalidate ./docs/knowledge` clean. Every claim in the moved text was checked
against the code it describes at its new heading.

`okfvalidate` is NOT the check that matters here, and it is worth saying why: it enforces the
dated H1 and the frontmatter, not whether a paragraph sits under the heading that owns it. It
passed on every revision of this entry, including the two that misattributed whole blocks. The
check that catches a scoping error is reading the rendered heading sequence —
`grep -n '^### '` — because a diff of an insertion shows only correct lines.

## The split had the defect it was fixing

Copilot found that the Google-Ads-specific material — the manager-id duplication,
`ListAccessibleCustomers`, the bootstrap lifecycle — was still rendering INSIDE `### Meta`. The
`### Meta` section landed ahead of that block rather than after it, so the split reproduced the
exact misattribution it exists to remove, one level down.

Worth writing down because of how it hid. The diff showed a correctly-formed `### Meta` heading
with correct Meta content beneath it; nothing in the added lines was wrong. What was wrong was
where the ADDITION sat relative to text it did not touch, which a diff cannot show and
`okfvalidate` does not check — it enforces the dated H1 and the frontmatter, not whether a
paragraph is under the heading that owns it. The check that would have caught it is reading the
rendered heading sequence, not the patch.

It took two attempts. The first added `### Meta` above the Google Ads block and assumed
everything after it was Meta's. The second reopened Google Ads and assumed everything after THAT
was Google's — but a Meta bootstrap block sits in the middle of it, so that was wrong the same
way. The section runs Google Ads → Meta → Google Ads → Meta → Google Ads → Shared, because the
material was written as ONE narrative when Google Ads was the only implementation and each later
provider was appended wherever it fit.

The lesson is about what to read, not about being more careful. A diff of an insertion cannot
show a scoping error, because every inserted line is correct in isolation; what is wrong is where
the insertion sits relative to text it does not touch. Only the rendered heading sequence shows
that, and checking it is one `grep -n '^### '`.

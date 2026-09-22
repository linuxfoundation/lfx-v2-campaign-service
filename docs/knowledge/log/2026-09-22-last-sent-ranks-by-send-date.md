# 2026-09-22 — `last-sent` finds recent sends, and orders them by send date

**Fix** — `GET /projects/{project_id}/audience-builder/last-sent` returned no recent
sends. Seven defects, all in this repo rather than in the portal data.

**A — the search needle was the whole event name, matched as one contiguous substring.**
`LastSentSearchTerms` returned `StripYear(eventName)` as a SINGLE term and
`hubspot.SearchEmails` applied it as one `strings.Contains` over name or subject. No
marketing email is named `"KubeCon + CloudNativeCon North America"`, so the sweep found
ZERO candidates for an event with a full history of sends. `brand_short` is optional, so
there was often no fallback term either. `KeywordOverlap` next to it tokenized properly —
the ranking had been built for a candidate set the search could not produce.

**B — ranking scored the NAME only, though the search matched name OR subject.** A
subject-only match scored zero and was truncated away behind emails that merely had
wordier names.

**C — `sent_at` could not influence the result.** `sendLists.PublishDate` was fetched
about 25 lines AFTER the truncation to `limit`, so the field the contract orders by
affected neither which rows survived nor their order.

**D — the sort key was the last EDIT.** `includedProperties` carried `updatedAt` and not
`publishDate`, so an ancient email touched last week ranked as the most recent send.

**E — `isPublished` prefix-matched `AUTOMATED`**, admitting `AUTOMATED_DRAFT` and
`AUTOMATED_SENDING` — a draft and an in-flight send counted as precedent, which is exactly
what its own doc comment said must never happen. (The prefix test had itself been the fix
for substring matching, which inverted the answer: `"UNPUBLISHED"` contains `"PUBLISHED"`.)

**F — `sent_at` was never validated** against the RFC 3339 shape the design documents.

**G — a failed selection read also lost the send date**, though when an email went out and
who it targeted are independent facts read from different places.

## What changed

`internal/platform/hubspot/email.go` — the paginated walk moved into `walkEmails`, shared
by `SearchEmails` (now only the substring match rule expressed over it, so the template
picker's published behaviour is identical BY CONSTRUCTION) and a new
`SearchEmailsMatching(ctx, accept EmailFilter)`. `publishDate` joined the projection and
`Email.PublishDate`; `ParseEmailTime` was exported and `sortEmailsByUpdatedDesc` now reuses
it instead of keeping a second parser.

`internal/audience/builder_resolve.go` — `LastSentSearchTerms` deleted (leaving it would
leave a trap that looks usable) and replaced by `LastSentTerms`/`MatchLastSent` plus a
curated `genericEventWords`. Admission needs ONE distinctive token or TWO of any kind. A
flat minimum-overlap count was rejected because it kills the motivating case:
`"KubeCon NA 2026"` overlaps a KubeCon event on exactly one token. Deriving distinctiveness
as real IDF over the scanned rows was also rejected — it forces a two-pass walk and a
stateful predicate for this fix's benefit.

`internal/dispatch/audience_explorer.go` — one sweep with that rule as the predicate;
ranking by parsed send date descending with overlap as the tiebreak and unknown dates last;
a brand-only PARTITION replacing a `break` (strictly stronger — the break only suppressed
the brand when an earlier term had matched, while the partition suppresses brand-only rows
wherever in the sweep the event match landed); an explicit `isPublished` allowlist plus
`sentInTheFuture` for `PUBLISHED_OR_SCHEDULED`; `sent_at` normalised on output; and a
re-sort after the fan-out.

`design/` and `gen/` are UNTOUCHED. `sent_at` and "Emails, most recently sent first"
already described the intended behaviour — this makes the implementation match the design,
not the reverse.

## Three things worth knowing

**The search query is never sent upstream.** `GET /marketing/v3/emails` carries only
`limit`, `sort`, `includedProperties` and `after`; matching is entirely client-side. So the
previous two-term loop re-read IDENTICAL pages and bought nothing but round trips. One walk
with a caller-supplied predicate is a net REDUCTION: up to 20 page GETs where there were 40.
The `Max(10)` fan-out budget is unchanged, and the send date now arrives on rows the sweep
already reads rather than costing anything extra.

**The projection is an optimization, not an assumption.** `publishDate` decodes as a plain
top-level scalar sibling of `id`, the same shape as `state`, so naming it in
`includedProperties` should return it — unlike the NESTED `to` object `GetEmailSendLists`
deliberately refuses to project. That distinction is now stated in the hubspot concept,
which otherwise read as self-contradictory. It has NOT been verified against a live portal.
It does not need to be for correctness: projecting the date buys correct SELECTION, and the
re-sort after the fan-out buys correct ORDERING even on a portal that returns every
projected date blank. `TestLastSent_OrdersTheReturnedRowsByTheAuthoritativeSendDate` pins
that degradation path so the projection cannot quietly become load-bearing. If the live
check shows the field is withheld, the contingency is a shortlist of `min(3*limit, 12)` for
`GetEmailSendLists` only, keeping the expensive `listBriefs` fan-out at the `limit`
survivors.

**`ErrSearchIncomplete` is now MORE likely, and that is the right side of the trade.** A
tokenized predicate is stricter than a substring, so it reaches the scan bound with nothing
accepted more often. A portal over `maxFilteredScan` (2000) emails whose event genuinely has
no prior send now returns a recoverable failure where it used to return `[]`. That is the
trade the hubspot layer already declares: an operator reads an empty panel as "this event
has never been emailed" and builds the next audience from scratch.

## Tests

New `internal/audience/builder_resolve_test.go` — the package had NO test for
`LastSentSearchTerms`, `StripYear` or `KeywordOverlap`, which is why defect A shipped. Nine
new cases across the three layers plus extensions to the existing projection, failed-list-read
and `isPublished` tests. The 14 existing `TestSearchEmails_*` cases pass UNEDITED, which is
the parity proof for the walk extraction: if one had needed editing, the extraction changed
behaviour.

Still to do before merge: one live
`GET /marketing/v3/emails?limit=1&includedProperties=publishDate&includedProperties=name`
against a real portal, and the endpoint itself against the portal that reproduced the bug.

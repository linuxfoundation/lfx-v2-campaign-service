# 2026-09-16 — LFXV2-2770: fourth review cycle hardens the untrusted boundaries

**Fix** — A fourth review pass over the audience-builder explore/compose work
([[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]], previously fixed in
[[2026-09-11-LFXV2-2770-third-review-cycle-for-the-explore-compose-endpoints]]) found defects
that share one shape: a boundary where something untrusted or unverified was treated as fact.

- **The event page reached the LLM prompt undelimited.** Scraped fields were concatenated into
  the extraction prompt, and the extracted `brandShort` reaches `EventKeywords` and
  `MasterListName` — so a page could steer which HubSpot lists a send is built from. The metadata
  is now quoted between `BEGIN/END PAGE METADATA` markers the system prompt declares untrusted,
  each field collapsed to one line with the markers neutralised and truncated, so injected text
  cannot close the block early. The model's reply is bounded on the way out for the same reason.

- **`bestMatch` returned on the first probe that matched anything**, making the synonyms in
  `EventSuppressionProbes` an ordered fallback rather than alternate spellings of one list. A
  stale `... Suppression` beat the current quarter's `... Exclusion`, which is the stale-list
  selection the newest-quarter rule exists to prevent. Ranking is now hoisted across all probes.

- **`LastSent` swallowed `ErrSearchIncomplete` on every term.** If every term hit its scan bound
  without matching, the function returned an empty list as an authoritative answer — the exact
  false absence the hubspot layer refuses to fabricate one level down. The error is now preserved
  and returned when nothing was found; a term that matches clears it.

- **A failed selection read rendered as an empty selection.** `GetEmailSendLists` failing left the
  row with two empty arrays — the same wire shape as a send that genuinely targeted nobody, so an
  outage became false precedent. A new `lists_unavailable` flag marks the row instead; the email
  is still shown, because its name and link are the operator's route into HubSpot.

- **`numberIDs` dropped blank ids silently.** `encoding/json` accepts `[1,null,3]` into
  `[]json.Number` with an empty middle value, so a malformed response reported two of three lists
  as a complete prior-send audience. It now fails the read, as `ListMembershipIDs` already did.

- **A blank exclusion was normalised away.** `exclude_list_ids: [" "]` composed a master with no
  suppression while the caller believed one was applied. Rejected on the raw input now, before
  `ExclusionIDs` can drop it. Inclusions keep their existing path, whose all-blank case still
  answers the wrapped `ErrNoInclusionLists` the handler maps to a 400.

Three docs described behaviour the service does not produce and were corrected: `api-catalog.md`
and `internal-dispatch.md` both claimed preview-count reports `25,000+` above the cap (it skips
the sweep and returns the summed sizes as an inexact upper bound), and `internal-service.md`
claimed the `ComposePartial` handler always passes a non-nil suppression. That last one was
load-bearing in the wrong direction: the BFF trusted it and keyed its discriminator on
`suppression.list_id`, so three of the four reachable shapes were rethrown as ordinary failures,
discarding the deterministic names an operator needs on a path that is not idempotent.

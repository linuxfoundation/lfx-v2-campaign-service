# 2026-11-25 — Newsletter review fixes: chosen references are gated, and four smaller ones

**Fix** — the one fix commit from the local pre-PR review of the newsletter and email/audience
change. Each item is a reviewer finding that was read against the code and confirmed.

- **A brief's chosen reference emails are read only from the project's own portal, and only when
  they are the brand's sent issues.** `event_details.referenceEmailIds` is caller-supplied, and
  `BuildReferenceBlockFromIDs` read any id it was given, on the shared LF connection too, and put
  the body in the model prompt. A caller could name a draft or another project's email. The shared
  connection is now refused outright, as the automatic lookup already does: the brand and the ids
  are both caller-written, so there they prove nothing about ownership. On the project's own
  connection each email must be a SENT issue of the brief's brand
  (`event_details.newsletterBrand`): `dispatch.IsSentBrandIssue`, passed to the source's
  constructor by `internal/container`. The cost: a project on the shared portal gets newsletter
  copy written from its notes without the previous issues' structure and voice. Staging is
  unchanged and still clones the previous issue.
- **A newsletter brief is always written with the newsletter prompt.** `generate-email-copy`
  chose its prompt from the optional `stage` parameter alone, so a newsletter brief requested
  without one got the legacy event prompt: no notes, no link rule. `emailCopyStage` resolves a
  newsletter brief to a newsletter stage whatever was sent, and never gives another surface's
  brief the newsletter prompt: `Newsletter` sent for an event, survey or paid brief resolves as an
  unrecognised stage.
- **The email wizard refuses a newsletter brief** (`refuseNewsletterWizard`, `400`). Its clone
  step replaces one rich-text block and applies the events UTM tagging, which would leave the
  previous issue's items in the draft. Newsletter issues are staged by the dispatcher.
- **A note link with userinfo or no hostname is not a link.** `normalizeLinkForMatch` accepted
  `https://user:secret@host/` and `https://trusted@evil/`; either would have been vouched and sent
  to every recipient.
  A `)` that closes a bracket opened inside the address is now kept
  (`.../wiki/Foo_(bar)`), the same rule the UI applies, so the two agree on what the notes link to.
- **The brand listing says what it reads.** `NewsletterBrands` dropped the walk's `bounded` flag.
  The walk covers the portal's most recently updated emails only; the Goa description and the API
  catalog now say so, the service logs a bounded walk, and a bounded walk with no sent issue is
  `ErrSearchIncomplete` rather than a cached empty list. `get-audience-brand-newsletters` is also
  documented as it behaves: the parsed brand must equal the given one, not contain it.
- **Losing the draft-retirement race no longer reports a deleted draft as reused.**
  `retireIfGoneUpstream` treated every failed delete as doubt and reused the row, so the loser of
  two concurrent creates returned the id both had just confirmed deleted. It now reads the slot
  again and answers from what is there: free, replaced by the winner, or still being staged.
- **`DescribeLists` no longer counts a missing list as a readable one**, so a missing list plus a
  refused one is the connection error it is, not a `200`.
- **The brand-listing cache is bound to the connection it was read with**, so a project that
  changes its HubSpot connection does not keep the previous portal's brands for 15 minutes.
- **The API catalog and architecture doc describe the newsletter surface**: the fourth
  `delivery_type`, its one stage, how `generate-email-copy` treats it, and the wizard's `400`.
- **Tests.** The dispatcher's phrase search is now driven end to end and asserted at compile time.
  Handler-captured state in the new HubSpot and copy tests crosses to the test
  goroutine under a lock or an atomic, and no handler calls `t.Fatalf`.

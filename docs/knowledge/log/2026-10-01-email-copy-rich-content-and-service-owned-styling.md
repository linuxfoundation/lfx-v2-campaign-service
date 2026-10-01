# 2026-10-01 — Email copy: richer content from the scraped facts, and service-owned styling

**Fix** — Generated email copy was thin in variant A and nearly empty in variant B. Five
independent causes, each verified against the running path rather than inferred:

1. `emailCopyEventDetails.Topics` read `json:"topics"`, but nothing on this path writes that key —
   the BFF sends `themes`. The field was always empty.
2. `speakers` was plumbed end to end and always arrived as `[]`, because the BFF's extraction prompt
   never asked the model for it.
3. The event-details blob already carried `audience`, `themes`, `formatNotes` and `sponsors`, and
   the generator read none of them.
4. Variant B sent `variant: undefined`, so it matched no variant block and got no section structure
   at all — the "few words" in the report.
5. Three of the four values in the frontend's `CAMPAIGN_EMAIL_VARIANTS` were never recognised
   upstream, and an unrecognised variant falls through silently to plain stage copy.

The shared system prompt also **asserted** that only `eventName`, `location` and `dates` are ever
supplied, which was false once the facts block existed and would have told the model to drop
sections it could in fact write. Rewritten to state the actual rule: a required section whose
supporting fact is absent cannot be written truthfully.

**Facts.** `eventFactsBlock` composes agenda, CFP, venue and sponsorship URLs, audience, format,
topics (`themes` merged with `topics`, case-insensitively de-duped), speakers, sponsors and the
description into one block. Every value is bounded BEFORE it is written — `maxFactURLRunes`,
`maxFactTextRunes`, `maxFactDescriptionRunes`, `maxFactListEntries` — so the block-level
`maxEventFactsBlockRunes` truncation is a backstop the per-field bounds make unreachable.
`TestEventFactsBlockHonoursItsBound` MEASURES that (3538 runes against 3600) instead of asserting
it, and fails if the backstop ever starts cutting, which would silently truncate the description
mid-sentence since it is written last. URLs run through `factURL`, which applies `httpURL` and then
DROPS rather than truncates — a truncated URL is a broken link, not a shorter one.

**Links.** The BFF now extracts `description`, `speakers`, `agenda_url`, `cfp_url`, `venue_url` and
`sponsorship_url`, and every URL it returns is checked by `verifyPageLink` against the set of
`href`s actually present in the fetched HTML. A plausible URL the extraction composed from the
site's shape rather than read from it is dropped. The Go side validates again via `factURL`.

**Variant B.** The `communityStoryVariant` case appends a second full variant block, eleven ordered sections
and a contrasting angle, and it explicitly forbids deadline, countdown, capacity and "last chance"
framing, so the A/B test measures framing rather than length. `CAMPAIGN_EMAIL_VARIANTS` shrank to
the two values the service recognises.

**Styling.** The model is told to write semantic markup and no styling; `styleEmailBodyHTML` drops
every attribute it wrote and re-dresses the surviving tags in this service's palette and type scale.
See [Email HTML Rewriting and Body Styling](../code/internal-service-email-html.md) for the tag
table, the email-client constraints behind it, and why the wizard sanitizer's tokenizer was
**extracted** into `rewriteHTML` rather than copied. Applied in `parseEmailCopyResponse`, at
generation time, because the BFF flattens `sections[]` into the one `body` string that both the
operator preview and the HubSpot draft are built from — so styling there is what makes the preview
faithful, with no second copy of the palette in TypeScript. `content.go` is untouched.

**Sizing.** `maxComposedPromptSize` moved from 14600 to 20400, its largest single move. Measured,
not estimated: the facts block contributes +3600 (producer-bounded and present on every stage-aware
request, so `worstStageFloorNamed` composes with a maximally-sized one), the community-story block
+305 over urgency-fomo (only one variant is ever appended, so a second variant moves WHICH one is
worst rather than the sum), and the shared stage-aware rules +1758 net, of which `bodyStyleRule` is
624. The worst case moved from Post-Event +urgency-fomo +alumni (11689) to Post-Event
+community-story +alumni (17352), so the worst valid composition is 19752 and 20400 clears it by
648 — just over the ~522-rune largest-single-section margin the constant's own note defines.

**Note** — `TestGenerateEmailCopy_ComposedBoundIsReachable` injected a 12000-rune stage template to
drive the composed bound. Raising the bound to 20400 silently stopped that overflowing, and the
test failed inside its own "should never be called" LLM handler rather than on the claim it makes.
Its template is now sized `maxComposedPromptSize+1`, which cannot fit under the bound for any value
of either.

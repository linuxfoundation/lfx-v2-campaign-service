# 2026-10-14 — Custom audience builder: pre-PR review fixes

**Fix** — found by the pre-PR review of the custom audience builder branch:

- **A brand the model named was overridden.** When the event page named no brand, interpretation
  fell back to `DominantBrand` over the event's own lists even when the model had named a brand and
  its master gate had already resolved, so the draft was silently re-gated on another brand's
  master. The fallback now runs only when neither the page nor the model named a brand
  (`TestInterpretCustom_TheModelsBrandBeatsTheListsFallback`).
- **Check 3 counted the audience against itself.** `RunQA` credited any readable send-time id as a
  suppression, including the audited list and lists it includes. Those no longer count.
- **HubSpot's validation reason is quoted in neutral words.** `customErr` said "rejected the list
  definition" for any VALIDATION_ERROR on the path, including preview's reads; it now says "rejected
  the request", and the mapping is tested with a real client error.
- **Truncation never splits a rune** (`blockSafe`, `validationMessageFrom`), and the prompt's block
  markers are neutralised in any case.

# 2026-11-13 — #575 Google call-to-action text is checked against the probed values

**Fix** — Demand Gen image ads and responsive display ads sent `callToActionText` as free
text, checked only against a 30-character cap. Google refuses values outside its fixed set
with `INVALID_CALL_TO_ACTION_TEXT` at `adGroupAds:mutate`, after the budget, campaign, ad group
and image assets exist. "Register" left a half-built paused campaign in prod on 7 Nov 2026.

- `displayCallToActions` and `demandGenCallToActions` (`call_to_action.go`) hold the values a
  validateOnly `adGroupAds:mutate` probe on 2026-11-13 (v23, account 8666746580) accepted.
  The probe copied a live ad of each type and varied only this field.
- The sets differ. Display alone accepts Install, Order now, Try now, Read more and Explore;
  Demand Gen alone accepts Donate now.
- `resolveCallToActionText` folds case and whitespace and sends the probed spelling. An
  unaccepted value is refused in the pure preflight, before any write. The 30-rune bounds
  (`maxDisplayCallToActionRunes`, `maxDemandGenCallToActionRunes`) are removed.
- Test fixtures that used "Register" or "Go" now use "Learn more".
- Tests: `TestCallToActions_AreTheProbedSets`,
  `TestResolveCallToActionText_SendsTheProbedSpelling` and
  `TestCreativeValidators_RefuseAnUnacceptedCallToAction`.

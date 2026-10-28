# 2026-10-27 — An explicit custom list name over 200 characters is refused, not cut (LFXV2-2770)

**Fix** — lfx-one review on PR #396. `FitListName` cut every custom list name to 200 runes,
including an operator-typed one, while the contract accepted 250: the `MaxListNameLength` refusal in
`freeCustomName` was unreachable, and two typed names differing past rune 200 collided into one
list's `(v2)`. Now `audience.CheckCustomListName` refuses an explicit name over 200 runes as a
`FieldError` (400 naming `name` and the limit) at the top of `PreviewCustom` and `CreateCustom`,
before any HubSpot call, and only the derived name is cut. The Goa design's `name` MaxLength is 200
(was 250; OpenAPI regenerated) and `CustomNameMaxLen` equals `MaxListNameLength`; the dead refusal
in `freeCustomName` is gone. lfx-self-serve's custom-builder name input has no max length on main or
the ss/supp-* branches, so the tighter contract refuses what it could send; the 400 names the
limit. Test: `TestPreviewCustom_RefusesAnExplicitNameHubSpotCannotSearch` (mutation-checked).

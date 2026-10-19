# 2026-10-14 — Custom audience builder: PR #337 review fixes

**Fix** — from the bot review of the custom audience builder PR:

- **The three custom endpoints were unroutable.** `custom/interpret`, `custom/preview` and
  `custom/create` were missing from the HTTPRoute regex and the Heimdall ruleset, so the deployed UI
  could not reach them. Both now admit exactly those three paths; `parity_test.go` pins them and
  refuses neighbours (`custom`, `custom/delete`, `custom/create/anything`).
- **A comparison the property type cannot take is refused at preview.** Every filter is built as a
  MULTISTRING or ENUMERATION operation, so `CheckFilterKinds` refuses a value comparison on a bool,
  number or date property, and `contains` on a picklist. `is known` / `is unknown` work on any type.
- **A speakers list is auto-filled only when it fits.** A filled-in list replaces the scope and
  edition filters, so `SpeakerListFits` requires a speakers list discovery would offer, no
  contradicting scope word ("past" for current-only, "current" for past-only) and every requested
  edition in its name.
- **Reuse never offers what discovery hides.** `ReusableCandidate` also applies `DiscoveryHidden`
  (suppression, bounce, test, staff, master).
- **A company or deal list is no send-time suppression.** QA skips a non-contact list, on the same
  object-type test `AttachExisting` uses.

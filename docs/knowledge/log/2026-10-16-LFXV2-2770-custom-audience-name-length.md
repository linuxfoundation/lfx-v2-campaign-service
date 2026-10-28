# 2026-10-16 — Custom audience names stay within HubSpot's 200-character limit

**Fix** — previewing a 17-city Karnataka audience returned a generic 500. The generated name joined
every city with " and " (264 characters), and HubSpot's list search, which checks whether the name
is taken, fails on any query over 200 characters (measured against the `tlf` portal: 200 passes,
201 fails).

- `CustomListName` names at most three places, then `and N more`.
- `freeCustomName` refuses any candidate over `audience.MaxListNameLength` as `ErrInvalidRequest`,
  so a long typed name is a 400 telling the operator to shorten it, not a 500.
- Tests: `TestCustomListName` (metro case), `TestPreviewCustom_RefusesANameHubSpotCannotSearch`.

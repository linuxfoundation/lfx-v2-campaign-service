# 2026-11-17 — #563 Microsoft gaps split: part 3, multiple ad groups

**Note** — The third PR of #613's split (branch `feat/563-microsoft-gaps-part3`) carries multiple
ad groups: `microsoftConfig.adGroups` behind `MICROSOFT_AD_GROUPS_ENABLED` (chart `"false"`), the
optional `service.BriefCreatePreflighter` hook for each group's display URL, the per-group toggle,
bid and keyword levers, the bid write's reconciliation by name with its settle window and
temporary 409, and the knowledge log entries for them. The ad-group parts of #613's mixed review
entries are restated in `2026-11-16-563-microsoft-ad-groups-confirmed-unconfirmed.md` and
`2026-11-16-563-microsoft-ad-groups-settle-window-preflight-display.md`.

It adds no picker and no conversion-goal code: those are part 2, built separately. The shared edits
(`microsoftConfig`, `pkg/constants`, `values.yaml`, `MicrosoftMarketingOpsInputs`) are additive.

One change beyond #613's code: `FindAdGroupByName`, the reconciliation's proof of absence, now
applies `decodeTrusted`'s identity check (`identityjson.Check`) to the whole ad group list before
streaming it, so an entry that declares `Name` twice under the decoder's case fold cannot read as
another name and hide the group (`TestFindAdGroupByName_TrustedListOnly`). The create's find-first
is unchanged.

The `config` attribute and create-campaigns method descriptions in `design/brief.go` are still not
updated with the Microsoft text. Both are single lines that regenerate into the OpenAPI files, and
the `config` line alone would grow from about 25 KB to about 38 KB, so `design/brief.go`'s diff
would pass the size at which GitHub stops showing a file's patch. They land in a separate part 4,
after parts 2 and 3, so the text describes only merged code. Until then the API catalog's
MicrosoftConfig block documents `adGroups`.

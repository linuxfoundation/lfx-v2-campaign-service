# 2026-10-15 — #208: Meta create Steps withhold URL-shaped geo entries

**Fix** — On a successful Meta create, the "Geo targets dropped" and "Geo targets skipped" Steps
lines joined the raw caller-supplied geo entries. A mixed list such as
`["US", "https://evil.example/reset?token=…"]` therefore saved the URL, token included, in the
create job's Steps. Both lines now render their entries through `callerValueList`, the
`domain.CallerValue` path the geo refusals already use. A URL-shaped entry reads
"(value withheld)" and every listed code is quoted: `…: (value withheld), "IR"` and `…: "SG"`.

This deliberately changes success-path Steps text. Codes that used to appear bare (`IR, CU`) are
now quoted (`"IR", "CU"`). The line order and the place after "Account verified" are unchanged.
Pinned by `TestCreateCampaignGeoStepsWithholdURLEntries` in `internal/platform/meta`. A
mutation that reverts the dropped line to the raw join fails it.

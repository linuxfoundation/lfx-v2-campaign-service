# 2026-11-05 — Every image asset create sends a name

**Fix** — Google refuses an image asset with no name (`NAME_REQUIRED_FOR_ASSET_TYPE`), and no
image asset create in this service set one, so Display, Demand Gen (image ad and video logo),
Performance Max and Search image creatives all failed in prod after the campaign existed. A
Display create from the self-serve Implement tab on 2026-11-05 left campaign 24431381015 with
its ad group and no ad. Every image asset create now sends `imageAssetName(data)`: the prefix
`LFX image ` (with its trailing space) plus 16 hex characters of the bytes' SHA-256. The name is deterministic, 26
characters long and free of the source URL. validateOnly probes the same day showed a named image asset is
accepted, names need not be unique, and 129 characters is refused. Tests assert the name on the
wire for all five paths. See `code/internal-platform-googleads.md` § Image asset names (#491).

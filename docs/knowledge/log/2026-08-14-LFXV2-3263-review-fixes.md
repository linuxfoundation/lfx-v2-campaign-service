# 2026-08-14 — LFXV2-3263 review fixes for the Reddit pixel and interest fallback

**Fix** — Four defects in the Reddit pixel and interest-fallback work, all found
by review of the commit that introduced them.

A stale comment block survived the pixel edit. The new rationale was appended
BELOW the old one rather than replacing it, so `CreateCampaign` documented both
the docs-shaped contract and its negation, back to back — and the surviving text
argued for the exact objective gate that made every UI create fail. The
`CampaignInput.ConversionPixelID` field doc and two payload comments carried the
same stale claim. All removed.

`TestCreateCampaign_PixelFallsBackToTheAccountConfig` passed against a wrong
implementation. The fixture gave `AccountConfig` the SAME string for `AccountID`
and `ConversionPixelID`, so mutating the fallback to read the account id left the
one test covering the connection fallback green. On the LF account those two
values genuinely coincide, which is what made the fixture look reasonable. It now
uses a distinct pixel and asserts the literal rather than the field it came from.
The `design/` example had the same defect — copied from `account_id`, so the
generated OpenAPI offered the advertiser id as the pixel id.

The surviving-interests step was unreachable. Every fallback fixture rejects
interests by construction, so the only line telling an operator their interest
targeting survived was never exercised, and a successful run emitted two separate
`Targeting:` steps. Folded into ONE line built from what is actually sent, with
two tests covering the surviving case — one where communities alone are rejected,
one where nothing is.

**Note** — A service-level round-trip test now pins the pixel through create →
read-back → update, including that a PUT omitting it CLEARS it. That is the
documented full-replace semantic rather than a bug, but an operator editing only
the connection label would break the next dispatch without touching anything that
looks related, so it is worth having recorded where someone will find it.

The migrations gained the MIT license header every other migration carries, and
the trailing newline the header script had stripped.

**Docs** — `gen/http/openapi*` was regenerated in the previous commit by running
`goa gen` directly, which skips the `cp` into
`cmd/campaign-service/kodata/gen/http/` that `make apigen` performs. `kodata` is
what the deployed pod serves, so the published spec omitted
`conversion_pixel_id` entirely while the code required it — a UI developer
reading the served contract could not have discovered the field. Regenerated
through `make apigen`; all four copies are byte-identical to `gen/http/` again.
Use the make target, not the bare tool.

**Docs** — A second review round found the DURABLE documentation had not been
updated, only the dated log. `CLAUDE.md:17` requires the concept file, and a log
fragment does not substitute for it: `docs/knowledge/code/internal-platform-reddit.md`
described only the communities fallback, and `docs/channel-connections-schema.md`
still said the Reddit table has no provider-specific columns. Both now describe
the two-dimension fallback and the `conversion_pixel_id` column.

**Fix** — The Steps trail conflated REJECTED with DROPPED. Reddit names one or
both dimensions in its 400; the retry then drops BOTH regardless, because
`baseTargeting` carries neither. Reporting "retrying without communities" while
also dropping interests told the operator Reddit had refused something it never
saw — and the two are re-added in different places in Reddit Ads Manager, so a
wrong attribution sends them to the wrong screen. The step now names what was
rejected and, separately, what was dropped alongside it.

**Note** — The `conversion_pixel_id` attribute stays OPTIONAL, and the trade is
now stated at the attribute rather than left to be discovered. PUT is a full
replace on every provider in this API, so omitting the field CLEARS a configured
pixel and the next dispatch is refused. That is the same semantic `account_id`
and `label` have always had — consistency across seven providers beats
special-casing one field — but it is sharper for a field a caller may not know
exists yet.

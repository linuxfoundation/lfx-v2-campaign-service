# 2026-08-24 — adoption was documented as the only way settings can diverge

**Docs** — five sites described the settings readback's whole reason for existing as adoption:
a campaign this service never created, bound to a brief, whose live configuration the recorded
request never matched. The design method description put it as "adoption (which is what lets
the recorded request and the live campaign disagree) exists only there", and `api-catalog.md`,
the dispatch knowledge page, `ErrSettingsReadbackUnsupported`'s godoc and the googleads
readback file header each carried their own wording of the same claim.

## Why the claim is wrong

`BriefService.UpdateCampaign` is a DB-only write. It loads the row, overlays `CampaignName`
unconditionally and `ConfigSnapshot` when the caller supplied a config, and persists through
`ReplaceCampaign`. No ad-platform call happens anywhere in the method — the file's own comment
on `ToggleCampaignStatus` says so directly, contrasting it as "Unlike UpdateCampaign (DB-only)".

`campaign_name` is a COMPARED field, and `advertising_channel_type` and the budget fields read
from the config snapshot. So an ordinary name or config edit moves the recorded side while the
platform keeps whatever it held, and the readback reports a divergence on a campaign that was
never adopted. Adoption is *a* path to divergence; it was never the only one.

## What the fix does and does not do

Prose only — no behaviour changed, and the comparison logic was already correct. The readback
never assumed adoption; only the documentation did.

The replacement wording deliberately describes the SHAPE — "more than one path lets them drift
apart" — rather than listing adoption and `UpdateCampaign`. An enumerating comment is a
standing liability: the list was accurate when adoption was the only writer, and a later
DB-only edit path silently falsified it without touching a line of the comment. A shape claim
stays true as paths are added.

The per-platform sentence keeps only what is verifiable from the code: Google Ads is the only
platform with a `SettingsReader` wired today, and the reason is the wiring, not adoption.

## The composite OpenAPI example contradicted itself

Goa synthesises an object example by cloning each attribute's own example. For
`CampaignSettingsReadback` that produced `fields` as the same `budget_amount` element repeated
four times, all `comparison: unknown`, beside `diverged_count: 1` and `unknown_count: 7` —
counts matching neither the list's length nor any verdict in it, and an object asserting a
diverged field none of its entries carried.

Fixed with an explicit `Example()` on the type, the same idiom `CampaignMetrics` already uses.
The attribute-level `Example(1)`/`Example(7)` are retained: they document each count's own
shape in the per-property schema, where no `fields` array sits beside them to contradict it.

**Adding an `Example()` reseeds goa's faker.** The placeholder strings in the generated CLI
files for audiences, briefs and connections all changed, though nothing about those types did.
Confirmed the generator is otherwise deterministic — `make apigen` on the untouched head
produces zero diff — and that the only STRUCTURAL change across the whole spec (examples
stripped, schemas and paths compared) is the settings endpoint's description. The churn is
noise to read past in review, not a contract change.

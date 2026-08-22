# 2026-08-22 — name the bound the code actually enforces

**Docs** — three references on the creative-upload path described a bound the code had stopped
enforcing, and one pointed at the wrong migration.

The upload gate was changed from a pixel-count cap to a decoded-BYTE budget weighted by colour
model, because the bound that matters is bytes allocated during `image.Decode` and the
pixels→bytes factor is a property of the decoded representation: Go decodes a 16-bit
colour-type-6 PNG to `*image.NRGBA64` at 8 bytes per pixel against an 8-bit image's 4. A
pixel-only cap therefore admits twice the memory it appears to for a 16-bit upload — which is
the defect the byte budget fixed.

Two comments still called it "the pixel cap":

- `internal/service/creative_asset.go`, stage 3's rationale for running stage 2 first;
- `docs/knowledge/code/internal-service.md`, the same sentence in prose.

Both now say decoded-byte budget and state the 16-bit asymmetry, because that asymmetry is the
whole reason the change was made and "pixel cap" hides it. The behaviour is already pinned —
`bytesPerPixelFor` has a table covering `RGBA64`, `NRGBA64` and `Gray16`, and the dimension table
asserts that 4000x4000 passes at 8-bit and fails at 16-bit — so this is the description catching
up with tested behaviour, not a behaviour change.

Separately, `creative_asset_test.go` attributed `UNIQUE (brief_id, checksum)` to migration
`000026`. That migration is the campaign-jobs retention index; the constraint is created by
`000028_create_creative_assets.up.sql`. A maintainer following the citation would have found an
unrelated migration and no constraint.

## The pattern

All four were found by reviewers reading a comment against the code beside it. A stale comment
survives every test — the code it describes keeps passing — so the only thing that catches it is
someone checking the sentence against the implementation. Worth noting the direction here: the
CODE was right in all four cases and only the descriptions were wrong, which is the variant that
review catches and CI never will.

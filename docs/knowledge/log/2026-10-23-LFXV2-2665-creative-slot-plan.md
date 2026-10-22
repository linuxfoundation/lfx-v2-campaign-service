# 2026-10-23 — Creative slot classifier and `get-asset-plan` (LFXV2-2665)

**Update** — A brief's creative library could be uploaded, listed and deleted, but nothing could
answer the question an operator actually has after a folder upload: *can this brief launch, and
if not, what exactly is missing?* Three new pieces answer it.

**`internal/creative` (new package)** holds the vocabulary and the matcher. `SlotSpec` states one
place a platform will accept an image — platform, channel, wire field name, operator label, a
list of acceptable `ShapeRule` geometries, a byte cap, and `MinCount`/`MaxCount` — and
`Classify` runs a library against a `Catalog`, returning assignments, gaps, the assets whose
dimensions are unknown, and the assets nothing will take. It imports no platform package; each
platform publishes its own rows.

Three decisions worth recording:

- **`AnyOfRule` exists because a reciprocal requirement is not a count.** Google requires Demand
  Gen's landscape marketing image *or* its square one, each only if the other is absent. A
  `MinCount` of 1 on both reports a gap for a shape Google does not need; 0 on both reports no
  gap for an ad Google will refuse. Both are wrong in a way an operator acts on. Performance
  Max, which requires both outright, is the contrast proving the distinction is real.
- **Zero dimensions mean UNKNOWN, not 0x0.** `Eligible` refuses an undimensioned asset rather
  than failing it against a shape, so a library stored before migration 000047 reads as
  *unclassified*, never as *the wrong shape* — the two are different answers and only one of
  them is fixed by a designer.
- **The gap reason is the deliverable.** "No eligible asset" is true and useless; a gap leads
  with the slot's `Describe()` so it names the shape and the pixel floor, and `slotGapReason`
  separates the three ways a slot can come up short, because the remedy differs: upload a
  different shape, upload more of that shape, or free a place in the shared ceiling first.

**`googleads.ImageSlotCatalog()`** publishes the fourteen Google slots across Performance Max,
Demand Gen and Display, built from `performanceMaxImageSlots`/`demandGenImageSlots`/
`displayImageSlots` rather than re-typed, with the per-channel shared marketing-image ceilings
and the two reciprocal rules.

**`get-asset-plan`** (`GET /projects/{project_id}/briefs/{brief_id}/creative-assets/plan`) is the
read. `slotCatalogs()` in `internal/service` is the registry, placed there because it is the one
layer that already imports every platform, so the classifier stays dependency-free and no binary
links six API clients to classify an image. The `platforms` filter is a repeatable parameter;
absent means all, unknown is dropped rather than refused (it publishes no slots, so it can
produce neither an assignment nor a gap) with the drop visible in the echoed list, and the
resolved set is sorted so a shared group ceiling is allocated identically across two identical
requests.

**Fix** — `shapeOf` was copying the table row's label onto the `ShapeRule` as well as the
`SlotSpec`, so every slot described itself twice ("Demand Gen logo image: Demand Gen logo image
1:1 (within 1%), at least 128x128"). Found while verifying a quoted example for the knowledge
bundle; the shape now carries a label only where a slot has several geometries, and
`TestImageSlotCatalog_DescribesEachSlotOnce` pins it.

The route needed pinning rather than reasoning about: `/creative-assets/plan` shares a prefix
with `/creative-assets/{asset_id}`, and the generated muxer is the only layer that decides
between them — the failure mode is a 404 that looks like a missing asset, so a test registers
both on a real `goahttp.NewMuxer()` and asserts the literal segment wins.

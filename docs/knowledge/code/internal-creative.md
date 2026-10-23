---
type: "Code Concept"
title: "Creative Slot Classifier (internal/creative)"
description: "Platform-agnostic matching of a brief's stored images against the slots an ad platform publishes: a shape/floor/ceiling spec language including either-or requirements no per-slot count can express, and a deterministic classifier that returns assignments, gaps naming the shape and the pixel floor, and the assets it could not place. Depends on no platform package, so each platform publishes its own catalog."
resource: "internal/creative"
---

# Creative Slot Classifier (`internal/creative`)

## Overview

An ad platform does not accept "some images". It accepts a landscape marketing image of at
least 600x314 at 1.91:1, up to twenty of them; a square one at 1:1; a logo at 1:1 and
optionally a wide one at 4:1 — each with its own floor, its own aspect tolerance and its own
ceiling, and some of them required only in combination with each other. An operator who has
just uploaded a folder has one question: **can this brief launch, and if not, what exactly is
still missing?**

`internal/creative` answers it. It holds the vocabulary for stating a platform's image rules
(`slot.go`) and the matcher that runs a library against them (`classify.go`).

**It imports no platform package, and that direction is the point.** Each platform publishes
its own rows — `googleads.ImageSlotCatalog()` — and
[internal/service](internal-service.md) assembles the registry, because that is the one layer
which already imports every platform. The classifier therefore never grows a dependency on a
platform client, and classifying one image does not link six API clients into the binary.

## The spec language (`slot.go`)

`SlotSpec` states one slot: the `Platform` and optional `Channel` that own it, the wire field
name (`Slot`), an operator-facing `Label`, a list of acceptable `Shapes`, a `MaxBytes` cap, and
`MinCount`/`MaxCount`. `Key()` joins platform, channel and slot into the identifier a client
addresses a row by — and a plan row joins back to a campaign config through it, which is why
`Slot` is taken verbatim from the channel's own table rather than derived from the label.

`Shapes` is a LIST because a slot can accept more than one geometry, and an asset satisfying
any one of them is eligible. Each `ShapeRule` holds the ratio as two integers (`RatioW`/
`RatioH`) rather than a float, so the documented 1.91:1 is literally `191/100` in the code and
cannot drift the way a hand-rounded `1.91` can, plus a `RatioTolerance` and optional
`MinWidth`/`MinHeight`/`MaxWidth`/`MaxHeight` bounds. Every bound is optional and **zero means
ABSENT, not zero** — no real image has a zero dimension, so the zero value carries "unbounded"
without colliding with anything a platform could state. The tolerance is **two-sided**, and the
drift from nominal is also what ranks candidates.

A `ShapeRule` carries a `Label` of its own only where a slot accepts several geometries and a
gap has to say which one was missing. With a single shape the slot's label is already the
subject of the sentence `Describe()` builds, and labelling both renders it twice.

Two rules are worth stating because they are easy to get backwards:

- **Width and Height are a PAIR, and zero means UNKNOWN, not 0x0.** `Eligible` refuses an
  asset whose dimensions are not known rather than failing it against a shape. An asset stored
  before migration 000047 carries no dimensions, so a pre-existing library reads as
  *unclassified* — never as *the wrong shape*. The two are different answers and only one of
  them is fixed by a designer; telling an operator their whole existing library is mis-sized
  would send them to redo work a dimension backfill does for free.
- **`MaxCount` is a per-slot ceiling; `GroupMax` is a shared one.** Google's marketing-image
  fields share a single budget across several slots, so the group ceiling is spent in catalog
  order and a slot can come up short because a sibling already spent it. The gap says so
  (`"combined ceiling"`), because "nothing qualifies" would send the operator to make an image
  that would also be refused.

### `AnyOfRule`: the requirement no count can express

Demand Gen and Display require a landscape marketing image **or** a square one. That is not
`MinCount: 1` on either slot — that would demand both — and it is not `MinCount: 0` on both,
which demands neither. Performance Max, which requires both outright, is the contrast that
proves the distinction is real rather than a modelling preference.

So `Catalog` carries `AnyOf []AnyOfRule` beside its `Slots`: the member slots' `Key()` values,
a `Min`, and a `Label`. A member key naming no slot in the catalog is ignored, so a platform
publishing a channel's cross-slot rules without that channel's slots cannot manufacture a gap
against nothing. A gap raised by an `AnyOfRule` names no `SlotKey` at all — the requirement belongs
to the group, and naming one member would send the operator to produce exactly the image they
did not need. `Merge` concatenates catalogs, preserving order, which is what keeps a
multi-platform request's shared-ceiling allocation reproducible.

## The classifier (`classify.go`)

`Classify(catalog, assets) Plan` returns four lists:

- **`Assignments`** — which asset ids fill which slot, one row per slot that got anything.
- **`Gaps`** — what is still missing, each with `Need` and a `Reason`.
- **`Unclassified`** — assets whose dimensions are unknown.
- **`Unused`** — assets with known dimensions that the plan did not place in any slot.

`Unused` is the mirror of a gap, and without it the plan answers only half the question after a
folder upload. It is **not placed**, which is wider than *not eligible*: a slot is filled to its
minimum and a group ceiling is shared, so a perfectly acceptable image lands here once the places
it qualified for are taken. The two readings are told apart by `Gaps` — nothing missing means what
is listed is surplus, not unusable.

**Ranking is closest-ratio, then larger, then stable.** The last tie-break is the asset's
`Ordinal`, which the service fills from the repository's oldest-first list index rather than a
timestamp — two files uploaded in one batch can share a `created_at`, and an index cannot. That
is what makes a reloaded plan identical to the one the operator was just looking at.

**A slot is filled to its MINIMUM, not its maximum.** `MaxCount` bounds what an operator may
attach; it is not a target the classifier should exhaust, or one eligible image would be spent
on the first slot that could take twenty. An asset may be reused across slots and never twice
within one.

**The gap reason is the deliverable.** "No eligible asset" is a true sentence and a useless
one; a gap leads with `SlotSpec.Describe()`, which states the shape and the floor —
`"Demand Gen marketing image: 191:100 (within 1%), at least 600x314, at most 5242880 bytes"` —
because
what the operator does next is hand it to a designer. `slotGapReason` has three arms (group
ceiling spent, nothing eligible at all, partially filled) and `anyOfGapReason` joins the
members' shapes with `"; or "`.

## Related

- [internal/platform/googleads](internal-platform-googleads.md) — `ImageSlotCatalog()`, the
  Google rows, built from the same slot tables the campaign-creation code posts.
- [internal/service](internal-service.md) — the platform registry and the `get-asset-plan`
  handler.

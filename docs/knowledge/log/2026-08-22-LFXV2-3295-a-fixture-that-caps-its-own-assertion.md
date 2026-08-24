# 2026-08-22 — a fixture that caps the number its own assertion tests

**Fix** — one vacuous assertion and three contract comments this branch falsified, plus
three defects noted in dated fragments and deliberately not rewritten.

## The assertion could not fail

`TestMeta_ResolveVariantAssetsBoundsAggregateBytes` built NINE 30-MiB assets and then asserted
`if n := assets.callCount(); n > 9`. The loop it had just written is what supplies the assets,
so nine calls is the ceiling the fixture itself imposes — `n > 9` was unreachable, and the
comment above it ("It must stop AT the ceiling rather than reading every asset first") described
behaviour nothing tested.

The behaviour is real: the running total crosses `maxVariantAssetBytes` (240 MiB) at the NINTH
read, and `resolveVariantAssets` returns there rather than buffering the rest. But an
implementation that read every named asset and checked the total afterwards would have been
equally green.

This was confirmed by mutation rather than by argument. Moving the ceiling check out of the loop
to just after it — so it still refuses, with the same error, and every existing assertion still
holds — left the ORIGINAL test passing. It is the strongest form of a survivor: the mutation
preserves the observable outcome and destroys only the property the test claimed to pin.

The fixture now supplies TEN assets to bound a NINE-read refusal, and asserts EQUALITY rather
than an upper bound. Fewer than nine would mean it refused before the budget was exceeded; ten
means the whole set was in memory before the check, which is the unbounded retention the ceiling
exists to prevent. The premise (that nine is where the ceiling trips) is asserted from the
constants, so moving the budget produces a direct message instead of a mysterious off-by-one.

**The general shape:** when a test asserts a bound on a count, check whether the fixture can
even produce a value that violates it. A loop bound and an assertion bound that are the same
number is the signature.

## Three comments this branch falsified

Adding `Ads []AdResult` to the persisted `CampaignResult` made ad ids persisted — while three
places still said they were not, and used that as the REASON for discovering ads via
`GET /{adSetID}/ads`: `internal/platform/meta/client.go`,
`internal/dispatch/meta.go` (twice) and `docs/knowledge/code/internal-platform-meta.md`.

The discovery is still right, but it is now a CHOICE rather than a necessity, and the reasons
are different ones: a live enumeration also covers ads added to the ad set after dispatch, and
it still works for result blobs written before the field existed. All three now say that. A
comment that gives a false reason for a correct behaviour is worse than none — the next person
to find `Ads` in the blob will read the discovery as an oversight and "fix" it.

## Noted in dated fragments, not rewritten

Dated log entries are history and are never edited, so these are recorded here instead:

- `2026-08-20-LFXV2-3295-creative-attach-by-bytes-or-url.md:10` says the multipart part is named
  `bytes`. The code sends `name="source"` (`internal/platform/meta/client.go`). That entry was
  already superseded by `2026-08-21-LFXV2-3295-adimages-part-name-and-one-entry-guard.md`, which
  establishes `source` as correct and `bytes` as the unsupported claim — so the record as a whole
  is right, and only the earlier entry read alone is misleading.
- The same entry's line 3 uses `**Change**`, which is outside the marker vocabulary CLAUDE.md
  lists (`Update`, `Fix`, `Creation`, `Note`, `Verification`, `Docs`).
- `2026-08-21-LFXV2-3295-adimages-part-name-and-one-entry-guard.md` carries no kind marker at all.

`okfvalidate` passes on all three, so none of them breaks the bundle; they are conventions the
validator does not enforce.

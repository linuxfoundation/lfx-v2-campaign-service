# 2026-08-24 — moving a decode boundary invalidates every comment that named it

**Fix** — the `Bytes` → base64 `String` change moved *where bytes get decoded*. Nine review
comments and several more sites still described the old boundary. Two of those stale comments
were not prose at all — they were load-bearing, and re-deriving them found a real memory defect.

## The real finding: pre-auth memory went UP, not down

`upload_admission.go`'s threat model justified the admission weights with "~72 MiB (the buffered
body plus the decoded byte slice)" allocated before auth. After the change:

| | before (`Bytes`) | after (`String`) |
| --- | --- | --- |
| pre-auth | 42 MiB body + ~30 MiB decoded slice = **~72 MiB** | 42 MiB body + ~40 MiB base64 **string** = **~82 MiB** |

Moving the decode *after* auth does not shrink the pre-auth half, because **the encoded string is
larger than the bytes it encodes**. The intuitive reading — "decoding moved later, so less happens
early" — is backwards.

Worse, the string is held by the payload struct, which stays reachable for the whole handler
(`p` is read again for `ProjectID`/`BriefID`). So it coexisted with the 80 MiB pixel buffer:

```
42 (body) + 40 (string) + 30 (decoded) + 80 (pixels) = 192 MiB
```

against `UploadAdmissionWeightBytes` = 128 MiB. **The admission bound was under-counting by
50%** — a bound that under-counts reads as protection while providing less than it appears to.

## Two ways to fix it, and why the constants did not move

The obvious repair is to raise the constants: weight 128 → 192, amplification 4 → 5. That was
tried and reverted, because it cascades. `UploadAdmissionBudgetBytes` (128 MiB) must be ≥ the
per-upload weight or every maximum-size upload 503s — the existing
`TestUploadAdmissionBudgetLeavesHeadroom` catches exactly that and did. Raising the budget to
192 MiB then breaks `DecodeAdmissionBudgetBytes`'s documented claim that the two budgets together
cap uploads at half the pod (192 + 128 = 320 MiB = 62%).

The cheaper and more honest fix is to stop holding the string: `decodeCreativeBytes` clears
`p.Bytes` the moment the decode returns — the only point it stops being needed. The peak returns
to ~128 MiB and **every constant keeps its existing derivation**. Prefer removing the allocation
over re-pricing it.

That clearing is now load-bearing, so it is pinned:
`TestUploadCreativeAsset_ReleasesTheEncodedStringAfterDecoding` fails if the assignment is
deleted as pointless. Asserting the field is cleared is the only observable proxy for "became
collectable" — Go offers no reachability assertion — and the test says so rather than implying a
heap measurement.

## `decodeCreativeBytes`: extracted, not renamed

`design/brief.go` referenced `decodeCreativeBytes in internal/service`. **No such symbol
existed** — the decode was inline. A design-doc reference to a helper that was never written
points a reviewer at nothing, and `design/` is the authoritative contract.

Extracted rather than corrected the reference, because the inline block was doing three things:
decoding, releasing the string, and enforcing the decoded ceiling. They are one transition and
each depends on the one before it — the release is only safe once the decode consumed the string,
the ceiling only meaningful once there is a decoded length. Separating them is what would let them
drift out of order.

## The aggregate reservation did not bind (suppressed finding)

The dispatch bound landed earlier reserved the aggregate **after** resolving every asset. Each
concurrent dispatch therefore materialised its full per-dispatch allowance and only *then* blocked
on the semaphore — five dispatches held ~1.2 GiB while queueing politely. **A reservation taken
after the allocation it accounts for is not a bound.**

Fixed by pricing each asset first: `GetAssetSize` reads one `BIGINT` and no `BYTEA`, the
reservation is taken from that figure, and only then is `GetAsset` called. Two consequences worth
noting:

- The per-dispatch ceiling now also trips on the SIZE, so the asset that crosses it is never
  loaded. The old "peak is the cap plus one materialised asset" overshoot is gone.
- An existing test asserted `GetAsset` was called exactly N times; it is now N-1, and that change
  *is* the property. Its comment already said the tripping asset must not be buffered.

`TestResolveVariantAssets_ReservesBeforeMaterialising` pins the ordering: with the budget held,
the resolve must be refused with **zero** `GetAsset` calls. Reverting the reserve to after the
load turns it red.

## The rule

**When a boundary moves, the invalidated claims are wherever anything describes *when* the work
happens, *what* a validator measures, or *what exists pre-auth* — not wherever the changed
identifier appears.** A grep for `Bytes` finds almost none of these; the sites say
"base64-decoded", "already-decoded slice", "the decoder has decoded it by the time". Sweep by
meaning, and treat a threat-model comment as arithmetic to re-derive rather than wording to
update — that is what turned a comment sweep into a real finding.

# 2026-08-24 — a ceiling that under-reports its own peak

**Docs** — `maxVariantAssetBytes` (240 MiB) bounds the distinct creative-asset bytes one Meta
dispatch may hold. The check read:

```go
// Charged once per DISTINCT asset, and checked BEFORE the buffer is retained so
// the ceiling bounds what this call holds rather than reporting after the fact.
totalBytes += int64(len(asset.Bytes))
if totalBytes > maxVariantAssetBytes {
```

"BEFORE the buffer is retained" is true of `byID`/`out` and false of the thing that actually
allocates. `GetAsset` scans the whole `BYTEA` out of Postgres, so the asset that *trips* the
ceiling is already resident in memory at the moment it is counted. The real peak is
`maxVariantAssetBytes` **plus one maximum-size asset**:

| | claimed | actual |
| --- | --- | --- |
| peak held by one dispatch | 240 MiB | **270 MiB** |

## Why an inaccurate comment here is worth a commit

The number is not load-bearing today — 30 MiB of overshoot against a 512 MiB pod is slack, and
the review graded it accordingly. It is worth fixing because of what the comment is *for*: a
future change sizes itself against the stated ceiling. Someone raising `maxVariantAssetBytes`
toward the pod limit reads "240 MiB is what this holds", lands on 480, and ships a 510 MiB peak
on a 512 MiB pod without ever making an arithmetic error. A bound that under-reports its own
worst case is exactly the input that turns a careful change into an OOM.

So the comment now states the overshoot, and states why it is accepted rather than closed:
reading `byte_size` before fetching the bytes would make the peak exact, but costs a second
round trip per asset on the dispatch path. The accepted-with-a-tripwire form is the point —
*"If the ceiling is ever raised toward the pod limit, close this gap first."*

## The stale citation on the same block

The same comment pointed at `design/brief.go's MaxLength(31457280)` for the 30 MiB per-asset
ceiling. That declaration no longer exists: the wire schema moved to `MaxLength(41943040)`, the
**base64-encoded** ceiling, and the 30 MiB **decoded** ceiling is `maxCreativeStoredBytes` in
`internal/service`. The block now cites the decoded ceiling by its real symbol and names the
encoded one as the same bound in the other unit.

This is the second half of the rule from
`2026-08-24-LFXV2-3295-a-number-is-stale-only-where-it-names-the-wrong-quantity.md`: the sweep
that fixed the `31457280` sites ran over `*.go` and `*.md` **on the upload branch**, and this
file arrived later, from the dispatch branch, carrying its own copy. A sweep is scoped to the
tree it ran against, and a merge can import fresh instances of a class already declared clean.

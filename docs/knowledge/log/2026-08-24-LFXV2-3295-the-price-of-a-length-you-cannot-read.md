# 2026-08-24 — the price of a length you cannot read

**Fix** — `UploadAdmissionWeightFor(-1)` is charged the worst-case CEILING again. The 8 MiB floor
introduced earlier the same day (`440723e7`) is reverted. Undeclared/chunked uploads therefore run
strictly one at a time, and a concurrent one is shed with a retryable 503.

This is a reversal, and both positions were reasonable. Recording why, because the next person to
read `UploadAdmissionWeightFor` will see an obvious throughput bug and be tempted to re-fix it.

## The two properties, and why they could not both be had

goa v3.25.3's `RequestEncoder` never sets `ContentLength`, so the *shipped generated client*
streams this upload chunked. Undeclared is not a rare branch here — it is the default path for the
only client that exists.

| price for `ContentLength < 0` | memory | concurrency (shipped client) |
| --- | --- | --- |
| ceiling (128 MiB = whole budget) | safe | **1** |
| floor (8 MiB) | **~480 MiB decoded, pre-auth** | 16 |

The floor was chosen first because concurrency-1 for the real client is a genuine availability
defect. It was reverted because the aggregate it admits is not survivable:

- `internal/service`'s `UploadCreativeAsset` keeps the base64-decoded slice (`p.Bytes`, up to
  ~31.5 MiB) live in `asset.Bytes` through `sha256Hex` **and** the whole `CreateAsset` insert. 16
  admitted uploads hold ~480 MiB of decoded slices alone, on a 512 MiB pod, before wire buffers.
- Nothing else bounds that sum. `MaxBodyBytes` caps ONE body, not the total.
  `DecodeAdmissionBudgetBytes` reserves only the PIXEL buffer and releases the instant
  `image.Decode` returns — it never covers `p.Bytes`.
- It is reachable **pre-auth**, because the middleware sits outside the mux by design and Goa
  decodes the body before `authJWTFn`.

## The rule this encodes

**A permit taken before the read cannot be priced from a length the server has not read.** The
floor charge is an assumption of "small" about a request that may be worst-case, and the permit
weight is the *only* control that bounds the aggregate. So the unmeasurable case must be charged
what the worst case costs.

A both-properties fix was looked for and does not exist at this budget: making the floor *honest*
would require capping a chunked body at ~2 MiB of wire, and a legal maximum upload is 40 MiB of
base64. It would reject every real upload.

The memory bound wins over throughput because it is the property this middleware exists to
provide, and because a 503 under concurrent uploads fails safe where an OOM takes the pod down for
every route. Uploads are low-frequency, so the cost is throughput, not correctness.

**The real fix is upstream, and it is not this constant.** Issue #183 makes the generated client
declare its encoded length; ordinary uploads are then priced by size and this branch becomes rare
instead of default. Anyone tempted to lower the charge here should land #183 instead.

## What pins it

The revert changes two tests, and neither is vacuous — reverting the revert turns both red:

- `TestUploadAdmissionWeightFor_PricesWithoutUnderCharging` — asserts the ceiling, asserts the
  weight is not below what a maximum-size declared body costs (an independent claim, not derived
  from the constant under test), and asserts `budget/weight == 1`.
- `TestUploadAdmission_UndeclaredLengthUploadsSerialize` — renamed from
  `...RunConcurrently`, which asserted the *opposite*. It parks one undeclared upload inside the
  handler, proves the budget is exhausted while it is parked, and requires the second to come back
  503. No sleeps, no elapsed-time thresholds.

The wiring test in `cmd/campaign-service` stayed green through both the change and the revert, by
design: it derives its parked count *from* `UploadAdmissionWeightFor`, so it self-adjusts from 16
to 1. Its comment already said so, and now says the reversal actually happened. That is the
intended split — a wiring proof must not break when pricing is retuned — but it means no failure
there is ever evidence about what an upload is charged.

## The class, not the line

The floor claim had spread into prose. Every site was swept in one pass, not just the flagged one:

- `pkg/constants/http.go` — the `UploadAdmissionWeightFor` doc block (rewritten to record the
  trade) **and** `UploadAdmissionMinWeightBytes`, which claimed to floor "a chunked request
  declaring nothing at all". It now floors DECLARED lengths only.
- `docs/knowledge/code/internal-middleware.md` — the "charged the FLOOR" section, plus the
  preceding "ordinary uploads run ~16 at a time" line, now scoped to declared lengths.
- `internal/middleware/body_limit_test.go` — a comment claiming to settle the question "for the
  pricing rule … whose floor for undeclared bodies rests on `MaxBodyBytes` bounds the real body
  regardless". That premise no longer applies to undeclared bodies; the surviving claim is scoped
  to the declared floor.
- `cmd/campaign-service/upload_admission_wiring_test.go` — a cross-reference to the old test name.

`docs/api-catalog.md` needed no change: it describes the 503 as a capacity refusal without naming
a price, which is what a contract doc should do.

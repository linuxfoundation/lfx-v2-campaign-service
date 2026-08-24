# 2026-08-24 — deduping the read left the upload undeduped

**Fix** — `resolveVariantAssets` made N variants naming one asset cost one DB read and one
buffer, and the work stopped there. `createVariantAd` still called `uploadImage` once per
variant, so five variants naming one 30 MiB asset pushed five identical 30 MiB transfers
inside `CreateCampaign`'s single deadline. The dedupe had moved the cost downstream rather
than removed it: the saving was real for memory and absent for the wire, and the fifth
upload is what times the campaign out while buying nothing — Meta content-addresses ad
images, so uploads 2..N re-derive a hash the first already returned.

The fix is a per-`CreateCampaign` cache keyed by a SHA-256 of the bytes. Three properties
were chosen deliberately, and each is the reason an alternative was rejected:

- **Per-campaign, not per-Client.** A `Client` can outlive one dispatch, so a longer-lived
  cache could hand back a hash for an image since deleted from the ad account — a dangling
  `image_hash` on a paid creative. The cache must not outlive the campaign it serves.
- **Keyed by CONTENT, not by asset id or slice identity.** The client never sees the asset
  id, and buffer aliasing is a dispatcher implementation detail a direct caller need not
  reproduce. A content digest matches exactly when the upload would be an upstream no-op.
- **Only unambiguous SUCCESS is cached.** Nothing else, in any form.

That last one is the load-bearing rule, and it is the one a plausible implementation gets
wrong. Memoizing a failure would convert ONE bad transfer into a campaign-wide failure:
every later variant naming that asset would fail without ever retrying, though the failure
may have been transient. The ambiguous case is sharper still — a timeout or 5xx may have
landed upstream, but no hash came back, so there is nothing sound to reuse; caching the
error would propagate a failure that may not exist, and there is no hash to cache instead.
Retrying is safe precisely because the endpoint is content-addressed. So the cache can only
ever make a variant do LESS work when an earlier variant provably succeeded, and can never
make a variant fail because a different variant failed.

The dedupe test alone does not prove this. A cache keyed on something coarser than the
bytes — a length, a size class — passes "N variants, one upload" while attaching variant
1's image to variant 2's paid creative. That is caught only by a case with DISTINCT assets
of EQUAL length; a first version of the fixture used 11- and 10-byte values, and the
length-keyed mutation survived until the fixture was corrected. The mutation that survives
is the finding: the fixture, not the code, was the weak part.

Two claims were corrected alongside it, both of the same kind — prose asserting a mechanism
that does not exist:

- The ambiguous-upload Step told the operator that "re-dispatching reuses it". It does not.
  An upload shortfall leaves `AdCount` below the requested count, `Dispatch` persists
  `created_degraded`, and `isReusableCampaign` accepts that status as terminal and
  REUSABLE — so a later dispatch returns the existing campaign without re-running any ad
  step. The Step read as "retry and the ad appears", so an operator would wait on an ad
  nothing will create. It now states that re-dispatch will NOT recreate the ad and that
  reconciliation is manual, while keeping the still-true half: the image is
  content-addressed and needs no cleanup.
- `validateVariantImage` claimed it refused before "any credential is used". False on the
  production path: `Dispatch` calls `resolveMetaCredentials` first, which loads, decrypts
  and decodes the stored token before a client is ever constructed. The validator's value
  is spend-safety — no campaign or ad set exists yet — not credential avoidance. The same
  sentence had been copied into `docs/api-catalog.md` and was corrected there too.

The stale "N variants hold N copies" note about dispatch-side memory was left in
`internal/middleware/upload_admission.go` and `docs/knowledge/code/internal-middleware.md`
after the read-dedupe landed. Both now describe the SHAPE — each distinct asset resolved
once, with a cap on total distinct bytes — rather than enumerating a count that the next
change falsifies again.

Separately, a review claim that the multipart part needs a filename EXTENSION was checked
and not adopted. Meta's `/adimages` reference documents only `bytes` and `copy_from` and
describes no multipart upload at all, so it is silent rather than prescriptive; the two
official SDKs disagree on the part name (Python `source0`, PHP `filename`); and the only
source asserting an exact-match extension rule was about bulk-upload spreadsheets, not this
edge. The filename IS load-bearing for one reason — Meta keys the `images` response map by
its basename — but this client reads the single entry BY VALUE and enforces a one-entry
count, so it never derives that key. `TestUploadImageAcceptsSingleEntryUnderAnyKey` already
pins that independence across `creative`, `creative.png` and an unrelated key. No change
was warranted, and asserting an undocumented requirement would have been the defect.

Two further defects on the same path were fixed once the dedupe made them visible.

The dedupe cache keyed on the caller's raw uuid SPELLING, and `uuid.Parse` accepts four
of them for one uuid — canonical, braced, URN and unhyphenated. So a config could name one
asset through several valid aliases and defeat the dedupe completely: each alias missed the
map, re-read the row, retained another buffer and was charged against the aggregate budget
again — the unbounded case the dedupe exists to prevent, reachable with no extra stored
data, and it would eventually produce a false "distinct creative assets" rejection naming
assets that are not distinct. The parsed uuid's canonical form is now the key and the
lookup value.

`uploadImage` also skipped the throttle retry. The repo rule is that retry eligibility is
an IDEMPOTENCY decision rather than a method test: `doCreate` passes `retryThrottle=false`
because Meta exposes no create idempotency key, so a repeated shed create can duplicate a
paid object. This upload does not share that property — it is content-addressed, so
repeating it re-derives the same hash and creates nothing — and it was verified
independently rather than taken from the review. Not retrying was the costlier default:
the campaign and ad set already exist by then, so a transient 429 dropped the variant into
a `created_degraded` campaign that no re-dispatch repairs. Only the throttle arm retries,
bounded by `retryMax` and honouring `Retry-After` with `do()`'s over-cap abort. Detection
uses both of `do()`'s signals, because Meta reports rate limiting as an HTTP 400 carrying
a Graph rate-limit code far more often than as a 429 — a status-only test would miss the
common shape.

The retry made the multipart body replayable, and that is its own hazard: a `bytes.Reader`
is consumed by the first send, so reusing one would post an EMPTY body on every retry — a
request rejected for a reason unrelated to the rate limit that caused the retry. The
encoded body is built once and replayed from a fresh reader per attempt, and the test
parses each attempt's body rather than counting requests, so the empty-replay bug fails
the test instead of passing it.

# 2026-08-24 — the envelope arm kept the gate the status arm lost

**Fix** — an earlier entry on this PR removed the `readErr == nil` gate from
`uploadImageAttempt`'s throttle classification, so a truncated **429** is retried instead of
returned as final. That fix was applied to the status-only arm and stopped there. The
Graph-**envelope** arm above it kept its gate:

```go
case readErr == nil && json.Unmarshal(raw, &env) == nil && env.Error != nil:
```

which leaves the more common throttle shape still terminal. Meta reports rate limiting as an
HTTP **400 carrying a Graph rate-limit code** far more often than as a 429 — that is the stated
reason both `do()` and this function consult `graphRateLimitCodes` at all. A 400 whose body is a
COMPLETE `{"error":{"code":4}}` followed by a connection closed early on a mismatched
`Content-Length` parses perfectly, but arrives with a non-nil `readErr`. It therefore skipped
the envelope arm, failed `status == 429`, fell to `default`, and was returned as `final`. The
campaign and ad set already exist by the time the upload runs, so that is a `created_degraded`
campaign no re-dispatch repairs — the exact outcome the retry was added to prevent, reached
through the arm the fix did not touch.

`do()` had it right the whole time and says why: *"A truncated read does NOT imply an unusable
envelope: the common shape is a complete JSON body followed by a connection closed early on a
mismatched Content-Length, so `raw` often parses."* It unmarshals on every non-2xx path before
consuming `readErr`. The upload path now does the same.

**A fix that carves out one exit path leaves the others behind.** The prior round reasoned about
the truncated-429 case and fixed the arm that case lands in. But `readErr != nil` is a condition
on the RESPONSE, not on a status, so it reaches every arm of the switch. Enumerating the arms
first — rather than the reported symptom — is what surfaces the second one. Both the test that
existed (`TestUploadImageRetriesTruncated429`) and the test added here
(`TestUploadImageRetriesTruncatedGraphThrottle`) describe one defect on two arms.

**A survivor, and the second half of the change.** Ungating the parse means a truncated body can
now populate `Type`/`Code`/`Message`. That is wanted, but `raw` holds only what arrived before
the connection closed, so fields Meta actually sent may be missing; a caller reading a bare
parsed envelope would treat "we never finished reading" as "Meta said exactly this". So a
truncated envelope keeps its parsed fields AND sets `EnvelopeUnreadable`. Deleting that flag
assignment initially changed **no test** — a live survivor. Both new tests were needed because
they pin different things: the throttle test asserts the retry (code 4), and
`TestUploadImageTruncatedEnvelopeIsMarkedUnreadable` uses code **100** — deliberately not a
rate-limit code — so the response stays final and the assertion is about the flag rather than
about retrying. The read diagnostic is also no longer written over Meta's own `Message` on this
arm, since a parsed message is strictly better operator output than "read response body:
unexpected EOF".

**Three documentation claims corrected in the same sweep**, each contradicted by code in this
same PR:

- `validateVariantImage`'s godoc refuted the old "runs before any credential is used" claim and
  then reintroduced it one sentence later for `resolveVariantAssets`. That is equally false:
  `Dispatch` calls `resolveMetaCredentials` as its first statement (`internal/dispatch/meta.go`
  line 411) and `resolveVariantAssets` only at line 475 — after the token is loaded, decrypted
  and decoded. `resolveVariantAssets`' own godoc already says "It is NOT a pre-credential
  boundary". **No** check on this path is a credential-avoidance boundary; they are pre-upstream
  and pre-spend.
- `uploadImage`'s outcome contract promised a non-2xx would carry "the Graph envelope (or a
  redacted body snippet)". The non-Graph branch deliberately withholds the body — this request's
  first multipart part is the caller's image, a reflecting proxy would place those bytes inside
  the snippet, and `redactSecrets` cannot recognise image bytes. The contract now names the
  divergence from `do()` as intentional and points at
  `TestUploadImageErrorNeverCarriesTheRequestBody`, so the privacy boundary is documented where
  a future edit would otherwise "restore parity" and reintroduce the leak.
- A dispatch test comment called the multipart upload "the documented `bytes` create parameter".
  `bytes` is Meta's documented **base64 scalar**; this request sends a multipart **file part**
  (named `source`) precisely to avoid the ~33% base64 inflation. `uploadImage`'s godoc already
  draws that distinction carefully, and the test comment contradicted it.

The last one is worth separating from a settled question. Whether the part must be *named*
`bytes`, and whether its filename needs an extension, were both investigated and refused: Meta
documents only `bytes` and `copy_from`, is silent on multipart, and the two official SDKs
disagree with each other (`source0` in Python, `filename` in PHP), so no name is the contract.
That verdict stands. What was wrong here is narrower and factual — describing the transport we
send as a parameter we do not send.

# 2026-10-01 HubSpot hero re-hosting retries when the origin serves AVIF or WebP

**Fix** — A campaign email's hero banner was missing from the HubSpot draft while the in-app
preview showed it. `uploadHeroImage` (`internal/dispatch/hubspot.go`) is best-effort: any failure
logs a WARN and returns `""`, and `RebuildEmailContent` only adds the `staging_banner` section when
the URL is non-empty. The failure it swallowed was `hubspot: source URL did not return a decodable
image: image: unknown format` from `downloadImage` (`internal/platform/hubspot/files.go`).

The source (an event page's `og:image`, a `.jpg` URL on a WordPress origin behind Cloudflare and
Varnish) was returning `Content-Type: image/avif`. The origin sends `Vary: Accept`, but Cloudflare had
cached the plain URL as AVIF and served that copy to every client whatever `Accept` it sent — a
cache-busting query string returned the origin's real JPEG, as did its WordPress size variants. A
browser renders AVIF natively, which is why the preview was fine. The standard library decodes only
JPEG, PNG and GIF, so `sniffImageFormat` — added with the decode-based allowlist so that bytes are
never re-hosted `PUBLIC_INDEXABLE` on the strength of their `Content-Type` alone — refused it.

`downloadImage` now sends `Accept: image/jpeg,image/png,image/gif;q=0.9,*/*;q=0.1` (it sent none) and,
when the response declares `image/avif` or `image/webp` and fails the sniff, retries ONCE with
`lfx_fmt=1` appended to the URL (existing query kept byte-for-byte). The single-fetch half moved into
`fetchImage`, so the retry runs through the same guarded client, redirect policy, size cap and
`image/*` check as the first fetch. HTML served as `image/png` is still refused after one request. A
failed retry reports the first response's error, which now names the declared type instead of only
"unknown format". `allowedImageFormats` is unchanged: no format was added and no decoder dependency
was taken.

Not fixed: an origin that serves ONLY AVIF or WebP is still refused and still yields a hero-less
email, because accepting one needs transcoding and an image-codec dependency. The dispatch result
also still carries no "hero dropped" warning — it is a WARN log line only, like the A/B-variant
failure. Both are separate decisions.

Tests (`files_test.go`): the retry succeeds for `image/avif`, `image/webp` and a mixed-case type with
parameters, asserting exactly two requests and the `Accept` header on both; end to end through
`UploadImage` the JPEG, not the AVIF, reaches HubSpot Files; a retry that also returns AVIF is
bounded at two requests; a retry that fails (a signed URL rejecting the extra parameter) reports the
first error without leaking the signature; undecodable `image/png`, `image/jpeg` and `image/svg+xml`
payloads are not retried; `withFormatRetryParam` is table-tested. `internal-platform-hubspot.md`
describes the behaviour.

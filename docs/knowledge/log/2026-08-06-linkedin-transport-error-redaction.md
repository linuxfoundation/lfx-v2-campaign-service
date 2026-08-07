# 2026-08-06 — LinkedIn metrics: redact credential-bearing transport errors before logging

**Update** — The transport layer still preserved untrusted error strings from `httpClient.Do` and response-body reads, matching the `costInUsdToMicros` vulnerability this PR already fixed. `http.Client.Do` returns `*url.Error` carrying the full analytics URL (with bearer tokens in the Authorization header), and injected `RoundTripper` implementations can include request/response headers or credential material in their error text. Both paths propagate to `transportError.Err`, which `BriefService.GetCampaignMetrics` logs verbatim.

Redacted via new `redactTransportError()` helper (metrics.go) that:
- Preserves classification sentinels (`context.Canceled`, `context.DeadlineExceeded`) so the caller can still classify unrecoverable retries
- Preserves pre-send dial errors (DNS, ECONNREFUSED, etc.) — they contain only network classification, not credentials
- Replaces all other errors (mid-flight, RoundTripper-injected) with a fixed safe message (`"analytics request failed"`)

Applied to both `httpClient.Do` error path (line 312-319) and response-body read error path (line 333).

Added `TestGetCampaignMetrics_TransportErrorRedaction` (metrics_test.go) with a fake `RoundTripper` that injects credential-bearing strings. Confirmed binding: reverting the redaction call causes the test to fail, showing the credential marker in the error string.

No behavioral change: the underlying error is still unwrappable for debugging via `errors.Unwrap`, only the logged string is safe.

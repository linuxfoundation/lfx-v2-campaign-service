# 2026-08-06 — LinkedIn metrics: redact credential-bearing transport errors before logging

**Update** — The transport layer preserved untrusted error strings from `httpClient.Do` and response-body reads, matching the `costInUsdToMicros` vulnerability this PR already fixed. `http.Client.Do` returns `*url.Error` carrying the full analytics URL with campaign/account URNs and query parameters, and injected `RoundTripper` implementations can include request/response headers or credential material in their error text. Both paths propagated to `transportError.Err`, which `BriefService.GetCampaignMetrics` logs verbatim.

Redacted via two new helpers (metrics.go):

`redactHTTPDoError()`: Handles `httpClient.Do` errors. Preserves classification sentinels (`context.Canceled`, `context.DeadlineExceeded`) and pre-send dial errors (DNS, ECONNREFUSED, etc.) by recursively unwrapping any `*url.Error` layers and returning the innermost dial error. This preserves `errors.Is`/`errors.As` classification for retryability while removing the URL. `http.Client.Do` may wrap a `RoundTripper` error in a `*url.Error`, and a `RoundTripper` itself may return a `*url.Error` — the recursive unwrap handles both layers. Mid-flight/RoundTripper errors (where no dial classification is reachable) return a fixed, safe message (`"analytics request failed"`). The cause is intentionally discarded; no `%w` verb, no errors.Unwrap reachability.

`redactBodyReadError()`: Handles response-body I/O errors from `buf.ReadFrom`, which are local failures after connection establishment and never carry credentials/URLs. Preserves cancellation sentinels for context, otherwise returns a distinct safe message (`"read response body failed"`) so callers can distinguish body-read failures from round-trip failures.

Added `TestGetCampaignMetrics_TransportErrorRedaction` with subtest `DialError_URL_redaction_preserves_classification` using a fake `RoundTripper` that returns a `*url.Error` wrapping a `*net.DNSError`. Verified binding: reverting the recursive unwrap causes test to fail showing the full URL; reverting the classification preservation causes DNSError to no longer appear in the error chain.

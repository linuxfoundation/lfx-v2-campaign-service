# 2026-08-06 — LinkedIn metrics: redact credential-bearing transport errors before logging

**Update** — The transport layer preserved untrusted error strings from `httpClient.Do` and response-body reads, matching the `costInUsdToMicros` vulnerability this PR already fixed. `http.Client.Do` returns `*url.Error` carrying the full analytics URL with campaign/account URNs and query parameters, and injected `RoundTripper` implementations can include request/response headers or credential material in their error text. Both paths propagated to `transportError.Err`, which `BriefService.GetCampaignMetrics` logs verbatim.

Redacted via two new helpers (metrics.go):

`redactHTTPDoError()`: Handles `httpClient.Do` errors. Preserves classification sentinels (`context.Canceled`, `context.DeadlineExceeded`) and pre-send dial errors (DNS, ECONNREFUSED, etc.) by recursively unwrapping any `*url.Error` layers and returning the innermost dial error. This preserves `errors.Is`/`errors.As` classification for retryability while removing the URL. `http.Client.Do` may wrap a `RoundTripper` error in a `*url.Error`, and a `RoundTripper` itself may return a `*url.Error` — the recursive unwrap handles both layers. Mid-flight/RoundTripper errors (where no dial classification is reachable) return a fixed, safe message (`"analytics request failed"`). The cause is intentionally discarded; no `%w` verb, no errors.Unwrap reachability.

`redactBodyReadError()`: Handles response-body I/O errors from `buf.ReadFrom`, which are local failures after connection establishment and never carry credentials/URLs. Preserves cancellation sentinels for context, otherwise returns a distinct safe message (`"read response body failed"`) so callers can distinguish body-read failures from round-trip failures.

Applied `redactBodyReadError()` consistently to both 2xx and 5xx branches of the body-read error path (metrics.go:353-360). The 5xx branch (`apiError.Body`) initially escaped redaction even though the same malicious/untrusted content justification applies regardless of status code — same error, same source, different error path. Now both branches call `redactBodyReadError` so the Body field contains the safe message in both cases.

Added `TestGetCampaignMetrics_TransportErrorRedaction` with subtests:
- `RoundTripper_credential_redaction`: Verifies mid-flight/RoundTripper credential markers are redacted
- `DialError_URL_redaction_preserves_classification`: Uses a fake `RoundTripper` returning `*url.Error` wrapping `*net.DNSError`, verifies URL is absent but classification is preserved
- `BodyReadError_redacted_in_5xx_path`: Verifies that `apiError.Body` contains the redacted message (extracted via `errors.As`) and not raw I/O error details
- `BodyReadError_redacted_in_2xx_path` (NEW): Verifies that transportError.Err (2xx body-read error path) contains the redacted message, not raw I/O error details

All binding verified: reverting each redaction branch causes its test to fail with the expected leak (full URL, lost classification, or raw I/O error in Body).

## json.Unmarshal redaction

**Issue**: `json.Unmarshal` errors can leak response body fragments via `json.UnmarshalTypeError.Value`, which may contain up to 10 MiB of malformed JSON from the response body. This reaches server logs in diagnostics and can expose untrusted content.

**Fix** (metrics.go:383): Changed from `fmt.Errorf("decode response: %w", err)` to `fmt.Errorf("decode response: malformed JSON (%d bytes)", len(buf.Bytes()))`. This preserves diagnostic utility (number of bytes received) while eliminating the error object that wraps the JSON fragment. Matches the pattern already used by `costInUsdToMicros` in this PR.

**Test**: Added `TestGetCampaignMetrics_MalformedJSONRedaction` which feeds a malformed JSON response with a credential marker embedded, then verifies (1) the marker is absent from the error message and (2) the safe message "malformed JSON" is present. Binding verified: reverting the fix causes the test to fail, leaking the marker in the raw error output.

## Retry test optimizations

**Issue**: `TestGetCampaignMetrics_RetriesOn429` and `TestGetCampaignMetrics_RetriesExhaustedReturnsError` consumed 1 second and 7 seconds of real wall-clock backoff time respectively, slowing test runs.

**Fix** (metrics_test.go:367, 408): Added `withRetryBaseDelay(time.Millisecond)` to both retry tests to use 1 millisecond backoff instead of the production 1-second default. This reduces combined test sleep from 8 seconds to 8 milliseconds without changing retry logic or reducing iteration count. Retry backoff is parameterized via the test harness and this confirms the backoff path is exercised without production-scale delays.

## Context-sentinel path bypassed redaction

**Issue**: Both `redactHTTPDoError` and `redactBodyReadError` opened with a combined early return
that handed back the ORIGINAL error whenever `errors.Is` matched `context.Canceled` or
`context.DeadlineExceeded`:

```go
if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
	return err
}
```

`errors.Is` matches straight THROUGH a `*url.Error` wrapper, and `http.Client.Do` wraps
cancellation and deadline failures in exactly that type. So on the cancellation path the helper
returned the wrapper intact, and `wrapper.Error()` renders the full analytics URL — including the
account and campaign query values — which `BriefService.GetCampaignMetrics` then logs verbatim. A
custom `RoundTripper` can likewise embed the Authorization header in its error text and wrap one of
these sentinels to reach the same path. The redaction was bypassed on precisely the branch that ran
first, in both helpers.

**Fix**: Return the CANONICAL sentinel rather than the untrusted outer error, in both helpers:

```go
if errors.Is(err, context.Canceled) {
	return context.Canceled
}
if errors.Is(err, context.DeadlineExceeded) {
	return context.DeadlineExceeded
}
```

`errors.Is` still matches for the caller's retry/cancellation classification, and nothing from the
wrapper — URL, query values, or injected header text — survives.

**Tests**: `TestRedactHTTPDoError/redacts_url_error_wrapping_context_canceled` and
`/redacts_url_error_wrapping_context_deadline_exceeded` build a `*url.Error` carrying a marker in
its `URL` field and wrapping each sentinel, then assert the marker is absent, `errors.Is` still
matches, and the result IS the canonical sentinel. `TestRedactBodyReadError` covers the same two
branches of the body-read helper. Binding verified: restoring the combined `return err` fails both
subtests with the marker rendered verbatim in the error string.

**Why it survived earlier rounds**: the two prior redaction fixes were each verified by re-reading
only the lines they changed. This branch preceded them and was never re-read, so two rounds of
review confirmed the file "clean" while the first exit of both helpers still leaked. Reported
independently by Cursor and Copilot.

## Dial cause hidden behind a fixed Error()

**Update** — The pre-send dial branch of `redactHTTPDoError` peeled the `*url.Error`
layers and then returned the innermost dial error itself. That removed the URL the
wrapper renders, but not the cause's own text: `WithHTTPClient` accepts an arbitrary
`RoundTripper`, so the innermost error is not this package's to trust — a custom
transport can return a dial-shaped error whose message, or whose `net.DNSError.Name`,
carries a URL or a credential. `transportError.Err` is exported and reaches
error-level logging, so that text would have been rendered verbatim.

The branch now returns a `*dialError`, whose `Error()` is the fixed string
`analytics request failed: connection error` and whose `Unwrap()` exposes the cause to
`errors.Is`/`errors.As` only. Classification (DNS, refused, timeout, and therefore the
retry decision) is unchanged; nothing from the cause is rendered. The `*url.Error`
peel is retained so no URL-bearing wrapper survives anywhere in the chain, not merely
at its head.

Bound by `TestRedactHTTPDoError/dial_error_text_never_reaches_the_error_string`, which
plants a credential marker in `net.DNSError.Name` and asserts it never appears in the
redacted error while `errors.As` still finds the `*net.DNSError`. Reverting the wrap
fails it with the marker in the diagnostic.

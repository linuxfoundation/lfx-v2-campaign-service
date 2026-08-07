# 2026-08-07 — LinkedIn metrics: default-deny dial cause, exact redaction assertions

**Update** — Closed the remaining review findings on PR #73 and the CI lint failure.

- **`errcheck` broke the build.** `internal/platform/linkedin/metrics_test.go` had one
  `fmt.Fprint(w, fmt.Sprintf(...))` whose return value was unchecked, unlike every other
  writer in the file. Now `_, _ = fmt.Fprintf(w, ...)`.
- **The dial-error redaction now rebuilds its cause instead of forwarding it.**
  `redactHTTPDoError` stripped `*url.Error` layers and kept whatever was underneath as
  `dialError.cause`. But the layer holding untrusted text need not be a `*url.Error`: a
  custom `RoundTripper` (`WithHTTPClient` takes an arbitrary one) can return
  `fmt.Errorf("Bearer <token>: %w", dnsErr)`, which `http.Client.Do` then wraps — peeling the
  outer layer leaves the credential-bearing `*fmt.wrapError` reachable by anything that walks
  or renders the chain. `safeDialCause` replaces it with a default-deny mapping: the three
  `syscall` sentinels are returned canonically, a `*net.DNSError` is rebuilt from its boolean
  classification bits alone (`Err`/`Name`/`Server` are exactly the fields an untrusted
  transport would use to smuggle text out), and anything else collapses to `errDialFailed`.
- **Two redaction tests could not fail.** `strings.Contains(x, "EOF") &&
  !strings.Contains(x, "read response body")` is unsatisfiable once the redacted prefix is
  present, so a leak that kept the prefix and appended raw I/O detail passed. Both now assert
  the COMPLETE sanitized value by equality.
- **`docs/knowledge/code/internal-dispatch.md`** named `GetCampaignMetrics` as the
  `MetricsReader` implementation; the implementation is `LinkedInDispatcher.ReadMetrics`,
  which delegates to that client helper.

**Verification** — reverted `safeDialCause` back to the unwrap-only form and re-ran
`TestRedactHTTPDoError`: it failed with `untrusted text survived into the redacted chain at
*fmt.wrapError: Bearer CREDENTIAL_MARKER_IN_WRAPPER: lookup
api.linkedin.com/?token=CREDENTIAL_MARKER_IN_NAME: no such host`. The new test walks every
layer of the returned chain, not just the outermost `Error()`, because `errors.As`/`Unwrap`
hand callers the inner layers directly.

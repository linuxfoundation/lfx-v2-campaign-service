# Platform HTTP client hygiene

Two request-layer patterns that recurred as each new ad-platform client was
added. Neither is caught by any linter here: `bodyclose` and `noctx` are not in
the default `golangci-lint` set, and no `.golangci.*` config adds them.

---

## platform-client-bounded-read-plus-one

**Severity:** `high`.

**Detect:** A response read bounded with `io.LimitReader(body, cap)` where `cap`
is the maximum acceptable size, with no explicit over-cap rejection. The repo's
form reads `cap+1` and then rejects `len > cap`:

```go
body, err := io.ReadAll(io.LimitReader(r, maxResponseBody+1))
...
if int64(len(body)) > maxResponseBody {
```

Flag the `cap`-exactly form, and flag a bounded read applied to **all** responses
including 2xx with no size check, which truncates a large valid JSON body and then
fails to unmarshal an API call that actually succeeded.

**Why it matters:** `io.LimitReader` reports **EOF**, not an error, when it hits
the limit. An oversized response whose first `cap` bytes happen to be valid JSON
therefore leaves `readErr == nil` and is accepted as a complete, successful
response. On a create path that means a truncated body is read as a successful
create.

**Evidence:**

- [`r3563330511`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563330511)
  (PR #20) states the mechanism exactly: "`io.LimitReader` reports EOF, not an
  error, when it reaches this limit. An oversized response whose first 10 MiB is
  valid JSON followed by extra data can therefore leave `readErr == nil` and be
  accepted as a complete successful response; the remainder is also closed without
  being consumed. Read one …". Fixed in `6e20f48`.
- [`r3562667503`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562667503)
  (PR #20) — the all-responses variant: "`doRequest` reads the response body with
  `io.LimitReader(..., drainLimit)` for *all* responses (including 2xx). This can
  truncate a valid JSON response >64KiB and then make `json.Unmarshal` fail, even
  though the API call succeeded." Fixed in `feb85ad`.
- Developer fixing commits on merged PRs: `d02b2780a` ("round-5 Copilot findings
  on Meta client") and then `fdfcf3101` ("detect truncated response") five minutes
  later on **#20**; `7ce9f1d` on **#22**.

**Status on main:** uniform across all seven platform packages, each with its own
cap: googleads `8<<20`, hubspot/linkedin/meta/reddit `10<<20`, twitter `1<<20`.
Named helpers are `readResponseBody` (`internal/platform/reddit/client.go:137`)
and `readAll` (`internal/platform/twitter/client.go:901`); linkedin and googleads
use `buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1))`.
`docs/knowledge/code/internal-platform-googleads.md:41-44` names it as "the
repo's standard discipline … bounded response reads (`maxResponseBytes+1`)".

**Not a finding when:** the read is a deliberate bounded *drain* whose content is
discarded — see the entry below, where reading only `cap` bytes is correct
because the bytes are thrown away. One knowledge-bundle concept describes a
client's bounded read as `maxResponseBytes` without the `+1`; the code is the
authority, and a reviewer citing that doc sentence would miss the boundary check.

---

## platform-client-drain-response-body-before-close

**Severity:** `should-fix`.

**Detect:** Any path that abandons a response body and closes it without reading
— in practice the 429 retry branch, and any early `return`/`continue` after a
non-2xx that will be retried. The repo's form is a bounded discard before
`Close()`:

```go
_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
```

**Why it matters:** closing an unread body prevents Go's HTTP transport from
reusing that connection, so every retry opens a new TCP/TLS connection to a
service that has just rate-limited us — adding latency and load exactly when the
platform asked us to back off.

**Evidence:**

- [`r3562542954`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562542954)
  (PR #20): "Closing a 429 response body without reading it to EOF prevents Go's
  HTTP transport from reusing that connection. Each retry can therefore open a new
  TCP/TLS connection while the service is already rate-limited, increasing latency
  and load. Drain the response body before closing it." Fixed in `90851c5`.
- Developer fixing commits on merged PRs: `a4ae5a7c9` on **#20**; `4ae520c`
  ("drain 429 body, bound token/Retry-After overflow, partial result") on **#21**;
  `7784fc6` on **#22**; `cd8a8f388` on **#35**, which introduced the named
  `drainAndClose` helper and cites the reddit client in its message.

**Status on main:** `drainAndClose` is a named helper at
`internal/platform/hubspot/client.go:568`, used at `:510,517,526`. googleads,
linkedin and reddit inline the same bounded discard.
**Known gap:** `internal/platform/twitter/client.go` still closes the 429
response body undrained on the retry path (`_ = resp.Body.Close()` at both the
attempt-exhausted and the retry branch, around lines 737-747). Treat twitter as a
site that has not adopted the pattern, not as evidence the pattern is optional.

**Not a finding when:** the body was already fully read, or the client and its
transport are demonstrably not reused after this call. The discarded `io.Copy`
error is intentional — every sibling that adopted the drain copied the discarding
form, so do not raise the unchecked error as a separate finding.

**"Terminal error return" is not an exemption**, and was wrongly listed as one
here. Terminal for the *call* is not terminal for the *transport*: returning an
error abandons the request, but the undrained connection belongs to an
`http.Transport` that outlives it and serves later requests — including through
the shared default transport. The exemption as written suppressed the known gap
recorded two paragraphs above, where twitter closes the 429 body undrained on the
attempt-exhausted branch: the most terminal path in the file, and the one the
entry cites as evidence the pattern is not optional. An exemption that silences
the entry's own provenance is not a narrow exemption, it is a hole.

Reuse must be *demonstrated* — a client constructed per call and dropped, or an
explicitly non-reused transport. Reaching a return statement does not demonstrate
it.

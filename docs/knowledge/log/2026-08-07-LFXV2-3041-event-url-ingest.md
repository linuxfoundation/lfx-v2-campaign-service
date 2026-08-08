# 2026-08-07 — Event URL ingestion, and where an SSRF check has to live

**Update** — `internal/platform/eventurl` fetches a caller-supplied event page over HTTP,
under an address guard and a size bound. It is the first step of the URL→brief path; the
parser that turns the returned bytes into `EventDetails`, and the endpoint that exposes
both, are their own PRs. No migration.

**Scope** — the fetch half only. The HTML/JSON-LD parser and the Goa endpoint follow as
their own PRs, split to stay under the 1000-line cap. The seams are not arbitrary. Review of
the endpoint turned up an untyped `Result(Any)` and a route missing from both the chart's
HTTPRoute regex and the `project-api` RuleSet — an endpoint that would have shipped
undeployable — so the API surface earns its own reviewable unit and its own parity test. And
fetching-a-hostile-URL and parsing-hostile-markup are different review problems: one is a
network-position question, the other a resource-exhaustion and contract question. They were
reviewed as one and each got the attention of half a PR.

This is the first place the service fetches a URL **the caller chooses**. Every other HTTP
client in the repo talks to a hardcoded platform host, so nothing was reusable and the
whole address-safety question is new.

## The check has to run at connect time, not before it

The first cut resolved the hostname with `LookupIPAddr`, rejected a loopback/private/
link-local/multicast result, then handed the original URL to `http.Client`. Its comment
claimed "an attacker can't redirect after the DNS resolution." That is backwards:
`http.Transport` resolves the hostname **again** when it dials, so check and connection
consult two separate DNS answers. A short-TTL record answers a public address for the
check and `127.0.0.1` for the connection, and the guard passes while the request lands on
the loopback interface.

The fix is `net.Dialer.Control`, which runs after resolution and immediately before the
connect syscall, on the address the kernel is about to use — and fires for every address a
multi-address host offers, not only the first. **General form: a check on a value that
will be re-derived before use is not a check.**

`Transport.Proxy` is left explicitly `nil` for the same reason. Substituting
`http.DefaultTransport.Clone()` would set `ProxyFromEnvironment`, and the dialer would
then only ever see the PROXY's address while the proxy fetched `169.254.169.254` for us.

## Rejecting the address and failing to reach it are different answers

`ErrEventURLForbidden` is a caller error and `ErrEventURLFetchFailed` an upstream one; the
handler that turns them into 400 and 503 lands with the API surface. The dial hook's
refusal surfaces from `client.Do` looking like any other transport error, so the forbidden
sentinel is re-checked **before** the generic branch. Flatten them and a request that must
never succeed is reported as retryable. The deny list enumerates what the `net.IP`
predicates miss — CGNAT, IETF protocol assignments, benchmarking, documentation, SRv6 SIDs,
and reserved (which carries the broadcast address); it is not default-deny, see below.
4-in-6 spellings are normalized first — `::ffff:169.254.169.254` is the metadata address in
different notation. Both sentinels wrap their cause with `%w`, so a cancelled fetch stays
`errors.Is`-able as `context.Canceled` rather than becoming an opaque upstream failure.

## The guard makes the obvious test unrunnable

`httptest` binds to `127.0.0.1`, precisely what the fetcher refuses, so no end-to-end test
of redirects, status handling or the size bound can run through `NewFetcher`. The
unguarded constructor lives in the **test file**, not as a parameter on the production
one, so no production path can build a fetcher without the guard. `isForbiddenIP` is a
pure function covered by table; only the `http://127.0.0.1:9/` case proves it is *wired*,
and reverting the hook fails that one with the wrong disposition rather than no error.

## The denylist calls itself a denylist

The address guard documented itself as default-deny. It is not — it enumerates special-use
ranges, so anything unenumerated stays reachable — and the comment mattered more than the
code, because a reader who believes a guard is default-deny stops checking the next range.
Two were missing: `IsUnspecified` matches only `0.0.0.0`, leaving the rest of `0/8` which a
Linux host routes locally, and `IsLinkLocalUnicast` is `fe80::/10` alone, so deprecated
site-local `fec0::/10` passed every predicate. A later pass over the IANA registry found
three more absent for the same reason — nothing in Go names them: `2001:db8::/32` and
`3fff::/20` (documentation) and `5f00::/16` (SRv6 SIDs). All are now listed and pinned by a
table test, so the next one is added by editing a table rather than by reasoning.

**A security comment that overstates the guarantee is worse than no comment, because it
retires the question.** Describe the mechanism you built and name the stronger one you did
not — here a destination allowlist, possible once the legitimate event hosts are known.


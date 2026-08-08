# 2026-08-07 — Event URL ingestion, and where an SSRF check has to live

**Update** — `internal/platform/eventurl` fetches a caller-supplied event page and parses
it into structured `EventDetails`. It is the first half of the URL→brief path: this package
only extracts, and generating copy/keywords/targeting from what it returns is a follow-up.
No migration — `event_details` already exists.

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
never succeed is reported as retryable. The deny list default-denies and covers what the
`net.IP` predicates miss: CGNAT, IETF protocol assignments, benchmarking, and reserved
(which carries the broadcast address). 4-in-6 spellings are normalized first —
`::ffff:169.254.169.254` is the metadata address in different notation.

## The guard makes the obvious test unrunnable

`httptest` binds to `127.0.0.1`, precisely what the fetcher refuses, so no end-to-end test
of redirects, status handling or the size bound can run through `NewFetcher`. The
unguarded constructor lives in the **test file**, not as a parameter on the production
one, so no production path can build a fetcher without the guard. `isForbiddenIP` is a
pure function covered by table; only the `http://127.0.0.1:9/` case proves it is *wired*,
and reverting the hook fails that one with the wrong disposition rather than no error.

## Parsing is the other half of the untrusted-input surface

Three strategies (JSON-LD, OpenGraph, `<title>`) share **one** `html.Parse` — the tree is
read-only, and parsing per strategy paid the most expensive step three times on a body
that may be 10MiB. Every extracted field is clamped on a rune boundary: without a bound a
hostile `<title>` puts megabytes into Postgres and into every brief built from it, and a
mid-rune cut turns that into a failed insert rather than a short field.

schema.org lets the same property be a string, a node object, or an array of either. A
top-level JSON-LD **array** is the common real shape — a page emits Organization,
BreadcrumbList and Event as one list — so unmarshalling into a `map` dropped the entire
block, and `location` as a Place array or `image` as an ImageObject came back empty with
nothing saying why. The failure mode is silent, which is what makes it worth naming here.

`extracted_from` records which strategy won: a brief built from a `<title>` deserves more
human scrutiny than one built from JSON-LD, and that is only visible if carried forward. A
page yielding no name reports `ErrEventDetailsEmpty` rather than an empty success — an
unreachable arm in a mapping table is indistinguishable from one that was never wired.

## The denylist calls itself a denylist

The address guard documented itself as default-deny. It is not — it enumerates special-use
ranges, so anything unenumerated stays reachable — and the comment mattered more than the
code, because a reader who believes a guard is default-deny stops checking the next range.
Two were missing: `IsUnspecified` matches only `0.0.0.0`, leaving the rest of `0/8` which a
Linux host routes locally, and `IsLinkLocalUnicast` is `fe80::/10` alone, so deprecated
site-local `fec0::/10` passed every predicate. Both are now listed with the rest of the IANA
special-use registry and pinned by a table test.

**A security comment that overstates the guarantee is worse than no comment, because it
retires the question.** Describe the mechanism you built and name the stronger one you did
not — here a destination allowlist, possible once the legitimate event hosts are known.


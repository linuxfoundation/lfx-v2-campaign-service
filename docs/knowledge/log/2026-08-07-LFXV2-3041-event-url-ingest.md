# 2026-08-07 — Event URL ingestion, and where an SSRF check has to live

**Update** — `POST /projects/{projectId}/fetch-event-url` fetches a caller-supplied event
page and parses it into structured `EventDetails`. It is the first half of the URL→brief
path: this endpoint only extracts, and generating copy/keywords/targeting from what it
returns is a follow-up. No migration — `event_details` already exists.

This is the first place the service fetches a URL **the caller chooses**. Every other HTTP
client in the repo talks to a hardcoded platform host, so nothing here was reusable and the
whole address-safety question is new.

## The check has to run at connect time, not before it

The first cut resolved the hostname with `LookupIPAddr`, rejected the result if it was
loopback/private/link-local/multicast, and then handed the original URL to `http.Client`.
Its comment claimed "an attacker can't redirect after the DNS resolution, so checking the
resolved IP is sufficient." That is exactly backwards. `http.Transport` resolves the
hostname **again** when it dials, so the check and the connection consult two separate DNS
answers. A record with a short TTL answers a public address for the check and `127.0.0.1`
for the connection, and the guard passes while the request lands on the loopback interface.

The fix is `net.Dialer.Control`, which runs after resolution and immediately before the
connect syscall, on the address the kernel is about to use. That answer **is** the one
inspected, and it fires for every address a multi-address host offers rather than only the
first. **General form: a check on a value that will be re-derived before use is not a
check.** Validate at the point of use, or hold the validated value and use that one.

## Rejecting the address and failing to reach it are different answers

`ErrEventURLForbidden` maps to 400 and `ErrEventURLFetchFailed` to 503. Keeping them apart
matters because the dial hook's refusal surfaces from `client.Do` looking like any other
transport error — so the forbidden sentinel has to be re-checked **before** the generic
branch. Flatten it and a request that must never succeed is reported as retryable, and the
caller is invited to retry it.

The deny list default-denies and covers what the `net.IP` predicates miss: CGNAT
(100.64.0.0/10), IETF protocol assignments (192.0.0.0/24), benchmarking (198.18.0.0/15) and
reserved (240.0.0.0/4, which carries the broadcast address). 4-in-6 spellings are
normalized first — `::ffff:169.254.169.254` is the metadata address wearing a different
notation, and an IPv4-shaped range test does not see through it on its own.

## The guard makes the obvious test unrunnable

`httptest` servers bind to `127.0.0.1`, which is precisely what the fetcher refuses, so no
end-to-end test of redirects, status handling or the size bound can run through the
production constructor. Hence `newFetcher(allowPrivate bool)`: `NewFetcher` always passes
false, and tests that need a live server pass true. The address decision itself is a pure
function (`isForbiddenIP`) tested by table, which covers ranges no live test could reach.

Both halves are needed. The table alone proves the predicate is right; only the
`http://127.0.0.1:9/` case proves it is **wired**. Reverting the hook leaves the table
green and fails that one with `connect: connection refused` — the wrong-disposition
outcome, not merely a missing error.

## Redirects are refused, not followed

`CheckRedirect` returns `http.ErrUseLastResponse`, so the 3xx is returned and rejected by
the non-2xx check. Following it would re-run the whole address decision against a location
the caller never supplied. The test asserts the redirect target was never contacted, with
`allowPrivate` on so that the assertion is about the redirect policy rather than the
address guard incidentally blocking the second hop.

## Extraction records its own provenance

The parser tries JSON-LD (`schema.org/Event`), then OpenGraph, then `<title>` plus
`meta[name=description]`, and stores which one won in `extracted_from`. A brief built from
a `<title>` deserves more human scrutiny than one built from JSON-LD, and that is only
visible if the source is carried forward. A page yielding no usable name is a 400, not an
empty `EventDetails` — an empty success would flow silently into brief generation.

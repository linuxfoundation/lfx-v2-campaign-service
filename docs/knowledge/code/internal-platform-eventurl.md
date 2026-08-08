---
type: "Code Concept"
title: "internal/platform/eventurl"
description: "Fetches a caller-supplied event page behind SSRF guards: a dial-time address check, no redirects, no proxy, and a bounded body."
resource: "internal/platform/eventurl"
---

# internal/platform/eventurl

Retrieves an event page so it can be turned into the `EventDetails` a brief is built from.
It is the first code in this service to fetch a URL **the caller chose**, which is what makes
this a security boundary rather than an HTTP call. Parsing the retrieved page into
`EventDetails`, and the endpoint that exposes both, land separately.

## The fetch is a server-side request forgery boundary

Every other outbound call in this service goes to a platform API at a base URL the service
owns. Here the caller names the host, so the request runs from inside the cluster with the
cluster's network position — reachable from it are the node metadata endpoint, the Postgres
service, and every internal address a browser could not touch.

- **The address check runs in `net.Dialer.Control`, not before the request.** Resolving the
  hostname separately and checking the result is a TOCTOU: `http.Transport` resolves again,
  so a short-TTL record can answer a public address for the check and `127.0.0.1` for the
  connection. `Control` runs after resolution and immediately before connect, on the very
  address the kernel is about to use — and on *every* address a multi-address host offers.
- **Redirects are not followed.** Following one would re-run the whole address decision on a
  location the caller never supplied. The redirect response is returned as-is and rejected by
  the non-2xx check.
- **`Transport.Proxy` is nil on purpose.** `http.DefaultTransport` sets `ProxyFromEnvironment`,
  so "simplifying" the transport would route every fetch through a cluster HTTP proxy — the
  dialer would then only ever see the *proxy's* address, and the guard would pass while the
  proxy fetched the metadata endpoint on our behalf.
- **The body is bounded at 10 MiB**, read one byte past the limit so an oversized page is
  *detected* rather than silently truncated into a parse of half a document.

`isForbiddenIP` is honestly a **denylist**, not a default-deny. Go's predicates are narrower
than their names suggest: `IsUnspecified` matches only `0.0.0.0`, leaving the rest of `0/8`,
and `IsLinkLocalUnicast` is `fe80::/10` alone, so deprecated site-local `fec0::/10` passes
every predicate. Both are enumerated in `forbiddenNets` and pinned by `TestIsForbiddenIP`, so
the next range is added by editing a table. A true default-deny needs a destination allowlist,
which becomes possible once the legitimate event hosts are known. **Over-rejection is a bug
too**, and its cost is worth stating precisely: `Fetch` fails with `ErrEventURLForbidden`,
which the endpoint answers **400** — so a legitimate public event page is refused outright,
and the message tells the operator the address is off limits. That is not a soft degradation
to "no metadata found"; it is a hard refusal blaming the caller for a URL that was fine.
Hence the public-address cases in the same test.

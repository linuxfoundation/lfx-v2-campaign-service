---
type: "Code Concept"
title: "internal/platform/eventurl"
description: "Fetches a caller-supplied event page behind SSRF guards and parses it into event details, using JSON-LD then OpenGraph then plain HTML."
resource: "internal/platform/eventurl"
---

# internal/platform/eventurl

Retrieves an event page so it can be turned into the `EventDetails` a brief is built from.
It is the first code in this service to fetch a URL **the caller chose**, which is what makes
this a security boundary rather than an HTTP call. Parsing the retrieved page into
`EventDetails` happens here too; the endpoint that exposes both lands separately.

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

**IPv4 embedded in an IPv6 address is the gap the range tables cannot see.** `net.IP.To4`
normalises exactly one embedding — IPv4-mapped `::ffff:0:0/96` — so every other notation
carries an IPv4 destination past both the predicates and the IPv4-shaped ranges. Three are
handled, in two different ways because a blanket deny costs differently. 6to4 (`2002::/16`,
bits 16-47) is deprecated by RFC 7526, so the whole prefix is simply refused and nothing
reachable is lost. The NAT64 well-known prefix (`64:ff9b::/96`) and RFC 2765's IPv4-translated
`::ffff:0:0:0/96` are NOT deprecated — on a NAT64/SIIT network the first is how a v6-only host
reaches the ordinary IPv4 internet — so denying them wholesale would refuse every legitimate
IPv4 event host. Those two are DECODED instead: `ipv4EmbeddingNets` extracts the low 32 bits
and re-runs `isForbiddenIP` on the IPv4 it names, which terminates because the decoded value
is four bytes and can no longer match a `/96`. `64:ff9b::a9fe:a9fe` is the cloud metadata
address; `64:ff9b::5db8:d822` is an ordinary public host and stays allowed. Both sides
of each rule are pinned by the public-address cases in the same test.

## Parsing degrades, it does not guess

Three strategies in strict precedence — JSON-LD `schema.org/Event`, then OpenGraph, then
`<title>` plus `meta[name=description]` — over **one** parse of the tree. Parsing per strategy
paid `html.Parse`, the most expensive step, three times on a body that may be 10 MiB.

`setIfEmpty` gives the field to the FIRST node that supplies it, so a page's `Organization`
block cannot overwrite the `Event`'s own name. That rule is only meaningful if traversal order
is document order, which is why `jsonLDNodes` pushes array elements onto its stack in reverse.

JSON-LD's shapes are the part that silently loses data if taken literally:

- The root is commonly an **array** (a page emits Organization, BreadcrumbList and Event as one
  list) or an `@graph` wrapper. Unmarshalling into a map drops the whole block.
- `@type` may be a string or an array of them, and matching on `event` as a substring catches
  the subtypes (`BusinessEvent`, `EducationEvent`) most conference pages actually emit.
- The same property may be a bare string, a node object, or an array of either — `location` is
  normally a `Place`, `image` an `ImageObject`. A plain assertion to `string` drops the majority
  shape and the field just comes back empty with nothing saying why.

Traversal is **iterative and explicitly bounded** (`maxJSONLDNodes`, `maxJSONLDDepth`).
`encoding/json`'s 10000-level nesting limit is not a usable bound: it caps what *decodes*, and a
document that decodes fine can describe far more graph nodes than any real page. The recursive
form also appended each child slice into its parent, so a nested `@graph` chain copied the
accumulated result once per level — quadratic allocation driven by attacker-controlled nesting.

## Every extracted field is clamped

Every field comes from attacker-controlled markup under a 10 MiB body limit, so without
clamping one `<title>` puts megabytes into a Postgres column and into every brief built from
it. `clamp` cuts on a **rune** boundary — a byte slice of UTF-8 can end mid-sequence and
Postgres rejects the invalid string, turning an oversized field into a failed request rather
than a short one. `clampFields` bounds every field in one place, so a new strategy cannot
introduce an unbounded one by forgetting.

## The JSON keys belong to the brief blob, not to this struct

`EventDetails` serializes as `eventName`, `location`, `startDate` … because those are the keys a
brief's stored `event_details` blob already uses. Consumers of that blob read `eventName`
specifically — `internal/dispatch/reddit.go`'s `briefFields` and `internal/service`'s
`briefEventDetails` both refuse a brief without it. Serializing the name as `name` produces a
result that stores cleanly through create-brief and then fails campaign dispatch, with nothing
between the two steps explaining why.

`URL` is deliberately **not** emitted as `registrationUrl`: the dispatchers treat that as the
link an ad sends a human to, and an event's landing page is often not its registration form.

`Parse` returns a zero `EventDetails` when nothing usable is found and the caller reports
`ErrEventDetailsEmpty`: a page yielding no name is a client error, not an empty success
flowing silently into brief generation.

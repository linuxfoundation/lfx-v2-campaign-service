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
carries an IPv4 destination past both the predicates and the IPv4-shaped ranges. Four are
handled, in two different ways because a blanket deny costs differently. 6to4 (`2002::/16`,
bits 16-47) is deprecated by RFC 7526 and IPv4-compatible (`::/96`, low 32 bits) by RFC 4291,
so both prefixes are simply refused and nothing reachable is lost — which is why `::5db8:d822`,
a public address in the deprecated form, is denied rather than decoded. The three `/96`s here
look alike and are disjoint: IPv4-compatible is twelve zero bytes, IPv4-mapped sets bytes
10-11, IPv4-translated sets bytes 8-9. The NAT64 well-known prefix (`64:ff9b::/96`) and RFC 2765's IPv4-translated
`::ffff:0:0:0/96` are NOT deprecated — on a NAT64/SIIT network the first is how a v6-only host
reaches the ordinary IPv4 internet — so denying them wholesale would refuse every legitimate
IPv4 event host. Those two are DECODED instead: `ipv4EmbeddingNets` extracts the low 32 bits
and re-runs `isForbiddenIP` on the IPv4 it names, which terminates because the decoded value
is four bytes and can no longer match a `/96`. `64:ff9b::a9fe:a9fe` is the cloud metadata
address; `64:ff9b::5db8:d822` is an ordinary public host and stays allowed. Both sides
of each rule are pinned by the public-address cases in the same test.

## The error chain carries only values this package minted

`Error()` renders no URL, and that is the easy half. `fetchError` also publishes an `Unwrap`,
so every value in its chain is recoverable by anything doing `errors.As` — logging middleware,
telemetry, a generic renderer. Holding the transport error there, even in an unexported field,
hands it out: `errors.As(err, &urlErr)` recovers the `*url.Error` and reads its **exported**
`URL` field, userinfo and query included. An unexported field is not a boundary when the type
publishes an `Unwrap`.

So the chain carries `safeIdentity(err)` instead: the canonical `context.Canceled`,
`context.DeadlineExceeded`, `io.EOF` and `io.ErrUnexpectedEOF` singletons and nothing else.
Those are field-less package-level values, so exposing one reveals nothing about the request,
and they are exactly what a caller distinguishing "we gave up" from "the page did not answer"
needs. Anything unrecognized contributes NO chain entry at all — the same default-deny
`safeCause` applies to the message, for the same reason: an unrecognized error is the case
whose contents are least vouched for. `*net.DNSError` and `*url.Error` would each have to be
rebuilt to be safe, and no caller branches on them.

## NAT64 has one prefix that can be known and an unbounded set that cannot

`64:ff9b::/96` (RFC 6052 §2.1) is the well-known translation prefix, and it is decoded
unconditionally. RFC 6052 §2.2 also permits **network-specific** prefixes at `/32`, `/40`,
`/48`, `/56` and `/64`, and those are the operator's own global unicast space — indistinguishable
by inspection from any other public prefix. On a cluster that uses one, the TRANSLATOR makes the
IPv4 connection, so an encoded `169.254.169.254` satisfies every check here.

They are therefore **configured**, via `NewFetcher(WithNAT64Prefixes(...))`, not guessed.
Speculatively decoding every address at all six layouts would over-reject: roughly one global
address in 256 has a zero octet at bits 64-71 and bytes that read as `10.0.0.0/8` at the `/64`
layout, and refusing a legitimate event page is a real cost, not a conservative default.

The decoding is per-length, because only `/96` puts the address in the low 32 bits — the shorter
layouts split it around the reserved octet at bits 64-71.

**Nothing the RFC merely requires to be zero is checked** — not that reserved octet, not the
trailing suffix bits. Every such check is a way to fail OPEN. `embeddedIPv4` returning nil means
"no embedded address here", and the guard's only two outcomes are refuse and dial, so a refusal
to *decode* is a decision to *connect*. Checking bits 64-71 would therefore hand an attacker a
one-byte bypass of the whole NAT64 check: set the reserved octet to `01`, and an address naming
`169.254.169.254` sails through.

The octet is not needed to identify the layout either. `embeddedIPv4` is only ever called after
an address matched a prefix whose length is KNOWN — from configuration, or from the well-known
`/96`. The length is given, not inferred, so there is nothing to self-describe. Nor is there
over-rejection to weigh against it: inside a matched translation prefix, every address is bound
for a translator by construction. (The over-rejection argument above is about *speculatively*
decoding arbitrary addresses, which is exactly why this package does not do that.)

**Residual risk, stated plainly:** an unlisted network-specific prefix is a live SSRF hole, and
this option is the in-process half of the answer, not a substitute for a destination policy at
an egress boundary. Where the prefix cannot be enumerated, the boundary is the only real control.

## Parsing degrades, it does not guess

Three strategies in strict precedence — JSON-LD `schema.org/Event`, then OpenGraph, then
`<title>` plus `meta[name=description]` — over **one** parse of the tree. Parsing per strategy
paid `html.Parse`, the most expensive step, three times on a body that may be 10 MiB.

`setIfEmpty` gives the field to the FIRST node that supplies it, so a page's `Organization`
block cannot overwrite the `Event`'s own name. That rule is only meaningful if traversal order
is document order, which is why `jsonLDNodes` pushes array elements onto its stack in reverse.

**Each strategy fills its own candidate; a losing strategy contributes nothing.** Threading one
`EventDetails` through all three looks like harmless reuse and is not: JSON-LD that yields a
description but no name loses, OpenGraph then supplies the name, and the record goes out stamped
`extracted_from="opengraph"` carrying a description no OpenGraph tag on the page contained. That
is two sources merged under one provenance label — and `extracted_from` exists precisely so a
human can judge how far to trust the rest of the record, so a label true of only some fields is
worse than no label at all. Provenance is all-or-nothing to mean anything.

The `<script type=...>` match parses the value as a **media type** and compares it without its
parameters. `type` is a media type, RFC 2045 permits parameters on one, and
`application/ld+json;profile="http://www.w3.org/ns/json-ld#compacted"` is the form the JSON-LD
spec itself defines. Comparing the whole attribute skips such a block and falls through to
weaker metadata — a silent quality regression, since nothing errors and the caller sees only a
thinner record. `mime.ParseMediaType` also folds case and trims surrounding space, so it
subsumes the previous `EqualFold`+`TrimSpace` handling.

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

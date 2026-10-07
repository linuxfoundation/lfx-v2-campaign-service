# 2026-10-06 — LFXV2-2665 creative fetch judged by the single eventurl address guard

**Fix** — The Google Ads creative fetch judged destination addresses with its own
predicate, `checkPublicIP`, rather than with `eventurl`'s. The enumeration it used —
unspecified, loopback, private, link-local, multicast, interface-local, CGNAT — was
wrong in a way no list of predicates catches: none of them decode an RFC 6052 address.
`64:ff9b::a9fe:a9fe`, the well-known NAT64 prefix naming `169.254.169.254`, matched no
IPv4 range test (`To4` normalises only `::ffff:0:0/96`) and no IPv6 predicate, so the
creative fetch would dial the cloud metadata service. Raised on PR #272 as a
network-specific NAT64 gap; the well-known prefix was open too.

`eventurl` already answered this, and its package doc already says why a second answer
is dangerous: "a second fetcher that builds its own `http.Client` is not a smaller
version of this one — it is an unguarded one." The failure reached this path through a
second GUARD rather than a second client, which the sentence did not anticipate and now
does.

The judgement is extracted as `judgeAddress` and exported as
`eventurl.NewAddressGuard(opts ...Option) func(net.IP) error`. `guardDialAddress` wraps
it for the `Control` hook, so the dial path and the predicate cannot drift.
`googleads.checkPublicIP` becomes `eventurl.NewAddressGuard()`. The creative fetch keeps
its own transport, which is why the predicate and not the whole client is what moves:
that transport refuses redirects with a REDACTED target, and its tests inject a TLS
config to reach an `httptest` server.

`googleads.WithNAT64Prefixes` mirrors `hubspot.WithNAT64Prefixes` and is wired from
`internal/container` alongside it, so a deployment that sets `EventURLNAT64Prefixes` now
declares them to all three guards rather than two.

Two things the tests pin deliberately. `TestCheckPublicIP` asserts
`64:ff9b::808:808` — public `8.8.8.8` through the same prefix — is still ALLOWED:
decoding a translation prefix must not become refusing it, which would refuse every
legitimate IPv4 creative host on a NAT64 network. And
`TestWithNAT64Prefixes_ReachesTheCreativeFetchGuard` asserts on ELAPSED TIME rather than
only on an error, because an undecodable address fails either way — a test asserting
just "it errored" stays green with the option deleted. Only refusal is immediate. This
is the same trap the HubSpot sibling's test documents, found by mutation there.

The swap is a strict strengthening: `isForbiddenIP` covers every class `checkPublicIP`
covered, plus the reserved and documentation ranges, plus the embedded-IPv4 decodings.
No address a legitimate creative host could occupy is newly refused.

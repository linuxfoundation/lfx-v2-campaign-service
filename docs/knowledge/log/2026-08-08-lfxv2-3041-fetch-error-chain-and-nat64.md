# 2026-08-08 — LFXV2-3041: the error CHAIN leaked what `Error()` redacted, and NAT64 has more than one prefix

Two findings on PR #92, both real, both verified before acting.

## 1. `Unwrap` handed out the `*url.Error` that `Error()` refused to print

`fetchError` redacted its message via `safeCause` and then stored the raw transport error in an
unexported `cause` field. Unexported is not a boundary: the type publishes `Unwrap`, so
`errors.As(err, &urlErr)` walks the chain, recovers the `*url.Error`, and reads its **exported**
`URL` field. Go redacts only the PASSWORD there — the username and every query value survive
verbatim.

Proven, not assumed. Reverting the fix and re-running the new test prints the recovered value:

```
fetcher_test.go:301: the *url.Error is reachable through the chain, carrying URL
    "http://s3cr3t:***@127.0.0.1:9/e?token=s3cr3t"
```

The chain now carries `safeIdentity(err)`: the canonical `context.Canceled`,
`context.DeadlineExceeded`, `io.EOF`, `io.ErrUnexpectedEOF` singletons and nothing else. Those are
field-less package-level values — exposing one reveals nothing about the request — and they are
exactly what a caller distinguishing "we gave up" from "the page did not answer" branches on.
Anything unrecognized contributes NO chain entry, the same default-deny that governs the message.

## 2. `64:ff9b::/96` is one NAT64 prefix, not the only one

The dial guard decoded only the well-known prefix (RFC 6052 §2.1). §2.2 also permits
network-specific prefixes at `/32`, `/40`, `/48`, `/56` and `/64`, carved from the operator's own
global unicast space and indistinguishable by inspection from any other public prefix. On a cluster
using one, the translator makes the IPv4 connection, so an encoded `169.254.169.254` passes every
check in this process.

The fix is `NewFetcher(WithNAT64Prefixes(...))` — **configured, not guessed**. Speculatively
decoding every address at all six layouts would over-reject: roughly one global address in 256 has
a zero octet at bits 64-71 and bytes reading as `10.0.0.0/8` at the `/64` layout, and refusing a
legitimate event page is a real cost. Over-rejection is a false answer too, not a safe default.

`embeddedIPv4` decodes per-length, because only `/96` puts the address in the low 32 bits; the
shorter layouts split it around the reserved octet at bits 64-71. That octet IS checked — it is
what makes a layout self-describing. The trailing suffix bits are deliberately NOT: a security
guard should not condition on a translator having honoured a MUST.

One test fixture had to be re-chosen. The first version used `2001:db8:1::/48`, which is the RFC
3849 documentation range already in `forbiddenNets` — the public-IPv4 case would have passed on
the wrong rejection. It now uses `2a01:4f8:1::/48`, ordinary global unicast, so the assertion is
about the NAT64 decode and nothing else.

**Residual risk, stated plainly:** an unlisted network-specific prefix is a live SSRF hole. This
option is the in-process half of the answer, not a substitute for a destination policy at an egress
boundary. The endpoint PR wires the option from chart config; where a prefix cannot be enumerated,
the boundary is the only real control.

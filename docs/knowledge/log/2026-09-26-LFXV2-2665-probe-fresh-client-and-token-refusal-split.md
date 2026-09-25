# 2026-09-26 — LFXV2-2665: the Reddit probe's own client, and splitting a token refusal from a credential verdict

**Fix** — three findings from the local review of `bbf2b91a`, all of them the same class of bug
the A1 connection-test work exists to remove: a connection reported as something it was not.

## 1. The Reddit probe answered from a cached access token

Reddit was the only one of six probes that took its client from `d.clients.buildOnce` instead of
building one. Two caches composed: `reddit.Client` holds its access token until the expiry
buffer, and `buildOnce` holds the client for the life of the connection row version. A probe
served from that cache authenticated with a token an earlier dispatch had minted and never
presented the stored refresh token at all — so a refresh token revoked an hour ago answered
`OK: true` until the access token aged out. That is the exact production failure this endpoint
was built to catch.

`resolveRedditClientWithCredsCache` makes the cache a caller's choice; `ProbeConnection` is the
only caller passing `useCache=false`, and it deliberately does not seed the cache on the way out
either. A parameter rather than a forked function, because the validations above the build are
the part a probe most needs to keep. `googleads`, `meta`, `hubspot`, `microsoft` and `twitter`
already construct their clients inside `ProbeConnection` — Reddit was the outlier, not the rule.

`internal/dispatch/probe_fresh_client_test.go` pins both directions and both were proved
non-vacuous against the pre-fix code.

## 2. Every non-429 sub-500 token status was a credential verdict

`googleads`, `reddit` and `microsoft` attached one sentinel to every non-`429` response below
`500`. That swept in three things that are not verdicts on the credential: a `3xx` (none of these
clients follow redirects, so a redirect surfaces as a status), a `404` or `405` from a moved
endpoint, and RFC 6749 §5.2's three REQUEST-shaped codes — `invalid_request`,
`unsupported_grant_type`, `invalid_scope`. Each is a failure of what this service sent, and each
told the operator to replace a credential the platform never looked at while the real defect went
unreported.

Worse, the sentinel was named `ErrTokenRequestRejected` — the name `domain.ErrTokenRequestRejected`
and `linkedin.ErrTokenRequestRejected` already used for the **opposite** meaning ("this is a
service defect, file a bug"). Reading one told you nothing about the other.

Each package now carries two sentinels and a `classifyTokenRefusal` that picks between them:
`ErrCredentialRejected` (the platform evaluated the stored credential and refused it — the only
class reported as a confirmed failed test) and `ErrTokenRequestRejected`, restored to the meaning
its name already had everywhere else and matching **neither** probe predicate, which routes it to
`domain.ErrServiceDefect`.

Two details are load-bearing:

- `ProbeInconclusive` gained an explicit `ErrTokenRequestRejected → false` arm in all three
  packages. That function answers `true` for anything it does not recognise, so without the arm
  the new sentinel would have inherited the default and reported `OK: true` with an advisory —
  unproven reported as healthy, the shape this whole ticket exists to remove.
- The fallback is deliberately **conservative**. Only a positively-identified RFC code, or a
  status no token endpoint answers a well-formed refresh with at all (`3xx`, `404`, `405`,
  anything else), is reclassified; an unrecognised body on a `400`/`401`/`403` stays a credential
  verdict. Promoting those would have turned the ordinary revoked-token case into a `500` that
  pages us instead of an answer the operator can act on.

Only the allowlisted `error` code is read out of the body, and it is compared against rather than
rendered — that request carried the client secret and the refresh token.

`token_refusal_test.go` in each of the three packages covers `invalid_grant`, `invalid_client`,
`unauthorized_client`, the three request-shaped codes, `302`/`404`/`405`, and the conservative
fallback, plus a routing test asserting the two sentinels reach opposite arms of `probeClass`.
Proved non-vacuous against the pre-fix classifier.

## 3. `docs/api-catalog.md` still described the pre-fix Google Ads probe

The catalog said the probe called `ListAccessibleCustomers` and that "the configured `account_id`
must appear in the answer" — the picker-membership behaviour that `ProbeAccountReach` replaced.
The row now names `ProbeAccountReach`, distinguishes flat mode (`ListAccessibleCustomers`,
membership) from manager mode (an unfiltered `customer_client` walk that reads the account's own
`manager` and `status`), states why the picker's filtered walk cannot be reused, and lists the
reached-manager and not-enabled verdicts alongside not-present-at-all.

Refs: LFXV2-2665

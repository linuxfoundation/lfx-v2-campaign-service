# 2026-09-26 — LFXV2-2665: the account-leg `408` gap, and what "not sent" actually means

**Fix** — Round 11 of the connection-probe work. Four findings; three landed, one declined with
its reasoning recorded here so it is not re-raised as an oversight.

## A `408` on the account leg paged us for a timeout

`googleads`, `microsoft` and `reddit` already read `408` as inconclusive on their TOKEN leg, and
say why in their own source: the endpoint, or an intermediary in front of it, gave up waiting for
the request, so nothing evaluated the credential and the same call can succeed on a retry — both
of which a rejection promises the opposite of. It is a timeout wearing a status code.

That reading never reached the other leg. Every platform's `ProbeInconclusive` recognised only
`429` and `>= 500` on its `apiError` arm, so an account-read `408` matched NEITHER predicate, fell
through `probeClass`'s default arm and became `domain.ErrServiceDefect` — a typed **500** that
pages us, for a transient upstream timeout an operator can do nothing about and that would likely
have cleared by the time anyone looked.

All six packages now include `http.StatusRequestTimeout` alongside `429` and `5xx`, and each
`TestProbePredicates` table grew an `api 408` row. The six rows were confirmed to FAIL against the
previous predicates before being kept.

The shared-helper suggestion was not taken: `apiError` is a distinct unexported type in each
package, and this repo's stated arrangement is that each platform package owns the reading of its
own errors. A helper would need an interface across six packages to remove one status comparison.

## "Not sent" means the FAILING request, not zero bytes

A probe of `googleads`, `microsoft` or `reddit` runs two legs. If the token refresh SUCCEEDS and
the account read then fails to dial, `ProbeNotSent` answers true and
`Orchestrator.ProbeConnection` suppresses the sample — even though one request did reach the
provider. Review read that as a contradiction of the sentinel's "nothing was sent" contract and
asked for the marker to be narrowed to a never-sent first leg.

The contract wording was wrong; the behaviour was not, and narrowing it would reopen the bug the
mechanism exists to prevent. `recordUpstream` is handed the probe's non-nil error, so the sample
being suppressed is an **error** sample. Recording it books this deployment's own DNS or egress
fault against the provider's error rate on
`campaign_upstream_call_duration_seconds` — precisely the inflation `probeReachedThePlatform`
was built to stop, and it would fire on every connection that platform has at once when a
cluster's resolver breaks. What suppression gives up instead is one SUCCESSFUL token call, which
hides no provider failure from anyone. A harmless undercount is the cheap direction; the miscount
is the expensive one.

So the wording moved rather than the behaviour: `ErrConnectionProbeNotAttempted` and all six
`ProbeNotSent` docs now say the DECIDING failure happened before its request left the process, the
sentinel's own text says the same, and the reason the two-leg case is deliberate is written down
where the next reviewer will find it before proposing the narrowing again.

## The LinkedIn catalog rationale, one round late

`docs/api-catalog.md:242` still justified `OK: false` on an incomplete LinkedIn walk with "did not
establish that the credential authenticated". Round 10's sweep missed it because it searched for
the claim's two earlier PHRASINGS, and this one states it as a negated clause. On this path the
claim is not merely imprecise but false: the credential baseline is what gates entry to the walk
at all, so LinkedIn had demonstrably accepted the credential.

Second time a paraphrase of the same claim survived a sweep. The durable lesson is not "search
harder" but that a claim duplicated across a design file, a generated catalog, two bundle
concepts, a domain sentinel, a service arm and a test comment will keep producing this round.

## Declined: the Microsoft `account_id` int64 range gap

Review asked that a 19-digit `account_id` above `MaxInt64` be rejected before persistence in
create/update. The gap is real and is already documented and asserted as deliberate —
`design/connection.go:1012-1015` names it as the one thing a `Pattern` cannot express, and
`TestValidateMicrosoftAdsConnectionConfig_IDPatterns` pins BOTH halves: the design validator
admits the overflow value, and `microsoft.ValidateAccountID` refuses it, with a comment saying the
test exists so nobody deletes the runtime check on the grounds that the design already bounds the
length.

Closing it in `internal/service` would need a new shared validator, because this service does not
import `internal/platform/*` — a layering change rather than a fix, and not one to make inside a
review round on a different endpoint. The harm is also bounded in the direction this branch cares
about: the connection stores, and then the connection test reports it as broken. Reporting an
unusable connection as broken is what this endpoint was built to do. Tracked separately.

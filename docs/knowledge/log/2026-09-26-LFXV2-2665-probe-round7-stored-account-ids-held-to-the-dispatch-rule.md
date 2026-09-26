# 2026-09-26 — LFXV2-2665: two probes that held a stored account id to a weaker rule than dispatch does

**Fix** — the seventh local-review round on the connection-test branch. The same defect on two
platforms, plus a correction to a rationale I had written wrongly twice.

## 1. Meta compared a stored id after stripping the prefix dispatch requires

`MetaDispatcher.ProbeConnection` ended in `probeMembership(reachable, trimMetaAccountPrefix)`,
which strips `act_` from BOTH sides. A legacy row storing the bare `123` therefore compared equal
to the enumerated `act_123` and the endpoint answered `OK: true` — while `MetaDispatcher.Dispatch`
hands the stored id to `meta.AccountConfig` untouched (`internal/dispatch/meta.go`, both the
create and discovery paths) and `meta.Client.CreateCampaign` rejects it on its own `accountIDRE`.
The godoc made this worse by stating the normalisation's purpose as keeping "a legacy bare-digits
row from being reported as unreachable" — describing the false positive as a feature.

The probe now calls `meta.ValidateAccountID` on the trimmed stored id before Meta is contacted and
answers `accountIDNotUsable`, the pre-send verdict. `trimMetaAccountPrefix` stays on the
comparison, but only the upstream side can differ now, and the godoc says so.
`TestMetaProbe_BareNumericStoredIDIsRefusedBeforeTheCall` deliberately serves `act_123` upstream,
so removing the guard and leaning on the normalisation again fails loudly rather than quietly;
`TestMetaProbe_CanonicalStoredIDStillPasses` pins that the guard refuses only what dispatch
refuses.

## 2. Microsoft's probe never reached the guard round 6 added

Round 6 exported `microsoft.ValidateAccountID` and had `Client.validateAccountIDs` call it — but
`MicrosoftDispatcher.ProbeConnection` builds its discovery client with `CustomerID` only, so that
method never runs on the probe path. `validateMicrosoftConnection` proves the id present, not that
it names an account, and `account_id` is operator-settable through the connection config API. A
stored `0`, or a 19-digit value above `MaxInt64`, therefore bought an upstream enumeration it
could not benefit from — and a transient 5xx on that enumeration classified inconclusive and
answered `OK: true` for a connection every campaign request deterministically rejects.

The probe now calls `ValidateAccountID` directly, immediately after `subject.accountID` is set and
before the `customer_id` check or anything upstream.
`TestMicrosoftProbe_UnusableStoredAccountIDIsRefusedBeforeTheCall` asserts the confirmed-failure
verdict, the `ErrConnectionProbeNotAttempted` marker, and — the part that matters —
**that nothing was sent**, against an upstream that would otherwise answer 503.

The generalisation, now stated in both the dispatch and microsoft concepts: exporting a validator
is not the same as applying it. A guard that lives on the dispatch client's constructor does not
protect a path that constructs a different client.

## 3. A rationale for the Reddit pixel verdict that was simply false

The round-5 comment justified `requiredConfigMissing` for a pixel-less connection with
"the service's own create path never fills that override in". That is wrong:
`RedditDispatcher.Dispatch` passes `redditConfig.conversionPixelId` straight into
`reddit.CampaignInput` (`internal/dispatch/reddit.go:178`), exactly as
`docs/api-catalog.md` documents it. The same sentence had been copied into
`probeSubject.requiredConfigMissing`'s godoc and into the dispatch concept, and a nearby comment
cited a test name that does not exist.

All four are corrected. The verdict itself is unchanged and the reasoning is restated honestly:
the override is the exception and not the configuration — the service supplies no default, so
every brief that omits the optional override is refused before any upstream call on a connection
configured this way, and reporting it healthy on the strength of a field each brief would have to
re-supply is the false positive the endpoint removes. The concept now also marks this as the one
probe verdict that is a judgement about the product rather than a certainty about the platform,
rather than presenting it as the only possible reading.

Refs: LFXV2-2665

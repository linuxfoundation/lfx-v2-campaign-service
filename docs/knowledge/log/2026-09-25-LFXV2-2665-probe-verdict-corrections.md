# 2026-09-25 Connection-probe verdicts: four ways they were wrong

**Fix** — Local review of the six-endpoint connection-probe change (same day, see
`2026-09-25-LFXV2-2665-six-connection-tests-verify-upstream.md`) found four defects in the
verdicts themselves. The probes ran; some of what they concluded was false.

## HubSpot failed working connections over a decorative field

The HubSpot probe compared `AuthenticatedPortalID` against `providerConfig["portal_id"]` and
answered `OK: false` — "the credential authenticates but does not reach account X" — on a
mismatch. The claim does not hold. `portal_id` is not an account selection and nothing routes on
it: its only readers are `email.go` and `lists.go`, which interpolate it into `app.hubspot.com`
deep links for assets that already exist. The portal a campaign lands in is the token's own,
derived by the client, which is the same fact `ReadMetrics`' provenance guard already records
when it refuses to use `portal_id` as a comparison basis.

So a blank, stale or never-filled `portal_id` described a connection that works, and the arm
failed it with a message that was untrue as well. The mismatch is now a warning log — a stale
value builds deep links into a portal the operator is not looking at, which is worth saying and
is a link-building defect, not a verdict.

The provenance framing in the original commit message, `docs/api-catalog.md`,
`internal-dispatch.md`, `internal-platform-hubspot.md` and that day's log entry was wrong for the
same reason and has been corrected in place.

## Every confirmed verdict stuttered the sentinel

`fmt.Errorf("%w: ...", domain.ErrConnectionProbeFailed, ...)` prepends the sentinel's own
sentence, and the confirmed-failure arm is the one class `internal/service` echoes verbatim
beneath its own prefix — so an operator read:

> connection found, but reddit ads verification failed: the connection failed verification
> against the platform: reddit ads rejected the stored credential for account t2_x

`confirmedProbeVerdictError` fixes it the way `linkedin.go`'s `confirmedOrgVerdictError` already
had: `Error()` forwards the authored text, `Is()` answers for the sentinel. The tag exists to be
matched, not read. `accountNotReachable` also rendered "does not reach for account X" —
`where()`'s trailing clause bent into a sentence object — so `whichAccount()` was added beside it
rather than bending one renderer to serve both positions.

## Two local config faults were reported as rejected credentials

`reddit.ProbeCredentialRejected` claimed `ErrInvalidAccountID` and
`twitter.ProbeCredentialRejected` claimed `ErrAccountNotConfigured`. Both are raised by the
client's own guard **before anything is sent**, so the platform never evaluated the credential;
the verdict told an operator to re-authorise a connection whose credential was fine and whose
account id was the only broken part. Both are now outside the predicates and intercepted by the
dispatcher next to its `ErrAccountNotSelected` arm — which is also what keeps them out of the
inconclusive default, where they would have answered `OK: true`. Reddit's gets its own verdict,
`accountIDNotUsable`, naming the field rather than the credential.

## Google Ads and Meta let an outage overrule a decided verdict

Both probed upstream before checking whether the connection names an account at all, leaving the
empty-account verdict to `probeMembership` — which is only reached when the enumeration
SUCCEEDS. An unrelated `5xx` on the way there classified inconclusive and answered `OK: true` for
a connection that cannot run a campaign under any circumstances. The check now runs immediately
after resolution, as Microsoft, Reddit and X already did through their `ErrAccountNotSelected`
arms. `TestProbeConnection_NoAccountIsDecidedBeforeTheCall` asserts both the verdict and that
nothing was sent, and was proved non-vacuous by removing the guard and watching it fail.

## The service-defect log said `unclassified`

`unusableConnectionReason` had no arm for `ErrConnectionProbeRequestRejected` or
`ErrConnectionProbeUnwired`, so every service-defect probe logged `reason=unclassified` — which
in that vocabulary means "no reason sentinel was attached", and there were two. The response for
that outcome is fixed text carrying no detail, so the token is the entire diagnostic. Added as
`probe_request_rejected` and `probe_unwired`.

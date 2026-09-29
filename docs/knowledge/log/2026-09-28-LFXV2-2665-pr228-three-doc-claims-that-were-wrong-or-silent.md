# 2026-09-28 — LFXV2-2665: three doc claims from PR #228 review that were wrong or silent

**Docs** — PR #228's bot review. No behaviour changes; three statements about behaviour corrected.

Worth recording because all three survived fifteen rounds of local review. Local review reads the
change; these were places where the change was right and the sentence describing it was not, which
is a different defect and apparently a harder one to see.

## `ErrConnectionProbeInconclusive` had the conjunction backwards

The sentinel's doc justified its `OK: false` with "an unverified credential is not an
authenticated one". That is false for every multi-leg probe. Google Ads, Microsoft and Reddit
refresh a token first and read the account second, so a probe can prove the credential
authenticated and *then* go inconclusive on the account read. The first half is established; only
the conjunction is not.

The distinction is not academic: "the credential did not authenticate" is
`ErrConnectionProbeFailed`'s claim, an operator acts on it by reissuing a credential, and this
sentinel exists precisely so that an unreachable platform is never reported that way. The doc
argued for the right answer from the reasoning of the defect the whole branch removes.

Now it states the conjunction and names the multi-leg case explicitly.

## `ConnectionProber`'s "required" read as required of all seven providers

The interface doc said a dispatcher not implementing it "is mis-wired", unconditionally. True of
everything that reaches `Orchestrator.ProbeConnection`; not true of the provider roster, because
LinkedIn's dispatcher does not implement it and is not missing anything — `TestLinkedinAds`
verifies through `OrgReferenceVerifier`/`VerifyAccountOrg`, which asks the probe's question and
adds the org cross-check on top.

The exception was recorded only in `probe_owned_resolver_test.go`. A reader of the doc alone could
"repair" LinkedIn by adding a `ProbeConnection` method and give the one endpoint with the stronger
check a second, weaker path to drift against. The doc and `internal-service.md` now both say so,
and point at the test that pins the six-implementation roster.

## HubSpot's unbounded `account_id` was silence where every sibling had a comment

This branch put a `Pattern` and `MaxLength` on Google Ads', Reddit's and Microsoft's ids.
HubSpot's `account_id` got neither and said nothing about why, which after a sweep like that reads
as the one field that was missed.

It is not. The bounds exist because those ids are interpolated into a request path, query or
header — shape is a transport concern before it is a preference. HubSpot's is stored on the row
and read by nothing: the campaign path takes its list id from `hubspotConfig`, and the connection
probe authenticates the token and compares `portal_id`. With no request for a malformed value to
reach, a bound would assert a shape this service has no way to know, and would refuse ids HubSpot
may legitimately issue. It earns one when a caller puts it in a request — which is what the
comment now says, in `design/connection.go` and in `design.md`'s asymmetry list.

## Declined from the same review

- **`project_id` at Warn level.** The reviewer withdrew it in its own text: consistent with
  existing practice throughout this service, not a new exposure.
- **HubSpot portal-mismatch warn noise.** Conditional on a symptom nobody has observed
  ("if this proves noisy in production"). A log level is cheap to change when it is.
- **Google Ads `MaxLength(64)` vs Microsoft's derived 18.** Different rules because the runtime
  checks differ: Google's client validates with `customerIDRE` and never parses to an int64, so
  there is no ceiling to mirror. Tightening it to look symmetric would invent a bound.
- **The 19-digit Microsoft ceiling.** Already deliberate, documented, and pinned by a test that
  names it as the accepted mismatch. Real ids are seven to nine digits.
- **`TestResult`'s description not being a per-provider catalog.** It illustrates the principle;
  it is not an index, and making it one puts seven provider behaviours in a field description that
  has to be re-edited every time one changes.
- **H1 em-dash formatting across the log entries.** `okfvalidate` passes; the mandated marker line
  is correct in every file.

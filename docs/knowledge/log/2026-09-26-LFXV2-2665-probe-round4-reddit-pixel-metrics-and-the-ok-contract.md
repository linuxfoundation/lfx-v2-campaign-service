# 2026-09-26 — LFXV2-2665: a Reddit connection that tests clean and cannot dispatch, an upstream series that counted local refusals, and an `ok` that promised too much

**Fix** — the fourth local-review round on the connection-test branch. Three fixes plus the two
stale explanations they made obsolete.

## 1. A Reddit connection with no conversion pixel passed its own test

`reddit.Client.CreateCampaign` refuses EVERY objective when the connection names no conversion
pixel — not only the documented `conversions` case; the live API was confirmed on 2026-08-13 to
reject a CLICKS create with `{"field":"conversion_pixel_id"}` — and it refuses before any upstream
call. The probe stopped at `VerifyAccount`, so a connection with a working credential and a
reachable account answered `OK: true` and then failed on first use. That is the exact failure
shape this endpoint exists to remove, reproduced by the endpoint that removes it.

`requiredConfigMissing` is the new verdict: the one that is about neither the credential nor the
account, for a field without which the platform rejects every create. It runs AFTER the
reachability check, not before it. A connection can be broken twice over, and answering the pixel
first would send an operator to fill in a field on a connection whose real problem is a dead
credential. That ordering also means a real upstream call has happened by the time the verdict is
reached, which is why — unlike the three pre-send verdicts — it is deliberately NOT marked
not-attempted. The probe reads the pixel through the new `reddit.Client.ConversionPixelID()`, the
same account config `CreateCampaign` reads, so the two cannot disagree about what "configured"
means. `probe_reddit_pixel_test.go` pins the verdict, its wording, the missing marker and the
ordering.

## 2. The upstream series still counted refusals the platform never saw

Round 3 excluded the three verdicts decided before a request is built. It did not exclude
everything the dispatcher's own resolver refuses — an unreadable row, an undecryptable credential,
an inactive connection, a malformed blob — all of which also happen inside the measured call, and
none of which the platform ever saw. A datastore or key-management incident therefore read as a
provider outage on the one series that is supposed to mean the provider.

The gate is now `probeReachedThePlatform`, which names the LOCAL outcomes — the not-attempted
marker, the unwired-dispatcher defect, and the credential resolver's own sentinel family — and
records everything else. Allow-listing the probe vocabulary instead was the first version and was
written out again: it drops whatever it does not recognise, so a post-call error a future
dispatcher returns outside that vocabulary would disappear from the series silently, which is the
expensive direction to be wrong in. Recording an unclassified error is the cheap one.

What makes that default safe is that the local set is checked rather than asserted:
`TestProbeLocalRefusalsCoverTheResolverVocabulary` scans `internal/dispatch/creds.go` for its
domain sentinels and requires each to be classified — in `probeLocalRefusalSentinels`, or in the
test's exemption map with the reason it is not a local refusal. Both sides are derived from
source, for the reason `recordUpstreamOperations` is: a hand-kept roster and a hand-kept gate move
together only by luck. Inconclusive outcomes stay recorded throughout — a call that was made and
failed in flight is the sample an on-call reader most needs.

## 3. `ok` promised an account is usable; several probes never check that

The Goa description said the credential authenticated AND the configured account is usable.
Microsoft and Meta test membership in an enumeration and knowingly accept accounts the platform
reports as suspended, paused or draft — `Usable()` is an allow-list on the Microsoft side
precisely so an unrecognised status is not read as healthy, and the probe still admits those
accounts on purpose, because they are states an operator fixes in the platform's UI and not by
re-saving a connection this service stored correctly.

The description is narrowed rather than the probes widened: "the credential authenticated AND the
configured account passed that provider's own check", with an explicit note that the depth of that
check is provider-specific and is not a guarantee of lifecycle state. Making six probes enforce
lifecycle usability to satisfy a sentence is the larger and worse change. Description-only, so the
regenerated `gen/**` and the four embedded OpenAPI copies carry no behavioural change.

## 4. Two explanations the round-3 fix had already made false

`docs/api-catalog.md`, `internal/dispatch/googleads.go` and
`internal/dispatch/probe_verdict_precision_test.go` all still said Google Ads `account_id` is the
one provider config with no `Pattern` at the design layer. Round 3 added that pattern. All three
now say what is actually true: the Goa boundary rejects the dashed form, and the runtime check
stays because Goa validates only the HTTP transport while bootstrap, migrations and rows written
earlier never pass through it.

`internal/service/connection.go`'s LinkedIn godoc still said the `testConn` credential-presence
caveat "still applies to the other 5 platforms". All six now probe. It says so, and says what
LinkedIn still has that they do not: an ad-account `reference` field to cross-check an org
against.

Refs: LFXV2-2665

# 2026-08-24 — a completeness guard that counts its own table proves nothing

**Fix** — upstream instrumentation coverage: `ReadCampaignSettings` records an upstream call
with `opReadSettings`, but `TestUpstreamCallsAreInstrumented` drove only the five prior
capability paths and its fake did not implement `SettingsReader`. Removing the call, passing
the wrong token, or inverting its success/error mapping would all have stayed green — the
exact failure mode instrumentation has when nothing asserts it.

The table now covers the path, but the durable half is how completeness is decided.
`recordUpstreamOperations` parses this package's non-test sources for the operation argument
of every `recordUpstream` call and resolves each identifier to its declared string constant,
so the roster moves in the commit that instruments a new path. A hand-written count could not:
both the count and the table are hand-maintained, so adding a path moves neither, and the
assertion ends up detecting edits to the test file rather than changes to the thing it tracks.
Same correction, and same reasoning, as the provenance coverage guard on LFXV2-3050.

The scan fails on an empty result instead of returning one, because a broken scan would make
the gate silently vacuous in precisely the way being fixed. It also fails if the operation
argument stops being a plain identifier, rather than quietly skipping what it cannot resolve.

Proved by control rather than by assertion. With the `recordUpstream` call deleted, the OLD
guard PASSED — a survivor — and the new one failed; and with a seventh instrumented token
added that no case drives, the derived gate named it. The first shows the guard now detects
what it exists to catch; the second shows it detects the case it could never have caught.

Also corrected here: `googleAdsCreationCustomerID`'s doc said an empty result meant callers
"must treat that as permission to proceed", which this PR's own fail-closed settings readback
falsified. Absence means UNKNOWN; what unknown PERMITS is a per-operation judgement, and the
two answers legitimately differ. The doc now states that shape and deliberately does not list
callers, since an enumeration is falsified by the next one added. Sweeping the sibling helpers
found Microsoft's and Meta's "unknown, proceed" wording still accurate — every one of their
callers permits absence — so only Google Ads' contract had actually split.

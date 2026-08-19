# 2026-08-18 — LFXV2-3050 the api-catalog's account-mismatch enumerations named the wrong adapters

**Fix** — extending the account-provenance guard to LinkedIn, Meta, Reddit and X/Twitter took the
number of adapters emitting `domain.ErrCampaignAccountMismatch` from three to seven, but
`docs/api-catalog.md` still enumerated the old set on both endpoints that answer with it.

**Two sentences asserted an adapter count the change falsified.** The toggle row
(`PATCH .../campaigns/{id}/status`) said "**Two reasons are Google-Ads-only on this path**",
naming an account mismatch as one of them "since Google Ads is the only adapter that verifies
account identity". The metrics row (`GET .../campaigns/{id}/metrics`) said "**Account-identity
mismatch is emitted by Google Ads and HubSpot, the only two adapters that verify tenant
identity**". Both now name the full set, and the toggle row's remaining Google-Ads-only reason is
counted as one rather than two.

**Both were already partly stale before this branch**, because `verifyMicrosoftAccountMatch` has
existed since LFXV2-2911 and neither sentence mentioned it. That is the tell for this class: an
enumeration drifts silently, one adapter at a time, and each change is individually small enough
that nobody re-reads the sentence. The count is what rots — not the explanation around it, which
was accurate and is preserved.

**The catalog is where a caller learns which platforms can answer 409 on these two endpoints**,
per `docs/architecture.md`: "the endpoint catalog with per-endpoint FGA relations is in
api-catalog.md". A consumer reading "Google-Ads-only" would not handle a 409 from a Reddit toggle,
which is now a reachable response.

**Both entries now also state the absent-provenance contract**, which the enumeration's phrasing
had left implicit: a row created before its adapter stamped the account records no provenance and
is waved through as "unknown", rather than being made un-pausable until a re-dispatch. Recording
it next to the mismatch is what stops a later reader from "hardening" the guard into failing
closed and stranding every pre-existing Reddit and X campaign behind a re-dispatch that spends
money. HubSpot's opposite choice is called out as the deliberate exception it is, since a bare
numeric email id carries no recoverable fallback to check against.

**No code changed.** The guard, its ordering ahead of each platform's narrower provisioning check,
and its tests are all as they shipped earlier in this branch; only the prose that describes which
adapters raise it moved.

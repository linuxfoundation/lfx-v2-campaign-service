# 2026-08-18 — LFXV2-3283 Google Ads geo targeting on both channels

**Fix** — Neither Google Ads create path attached location criteria. `CampaignInput` carried no
geo field at all, so every campaign created through the service served wherever the ad ACCOUNT's
defaults allowed. For an event campaign — the primary use case — that spends most of the budget
outside the region it was bought for. LinkedIn, Meta and Reddit all resolve and attach geo; Google
was the one platform that did not.

`geo.go` adds it for both channels: `CampaignInput.GeoTargets` takes ISO 3166-1 alpha-2 codes (the
vocabulary `metaConfig`/`redditConfig` already use), `validateGeoTargets` resolves each to Google's
numeric geo target constant, and the criteria are attached per channel. `internal/dispatch` gains
`geoTargets` on `googleAdsConfig`.

**The level difference is the whole trap, and it is why this was one ticket rather than a Demand
Gen fix.** Search takes CAMPAIGN-level location criteria; Demand Gen rejects those and takes the
same criterion on the AD GROUP. A single implementation attaching at the campaign level works on
Search and is refused on Demand Gen — *after* the budget and campaign have been created and cost
money. So there are two payload types and two functions named for their level, not one function
with a level parameter a caller could pass wrongly. Both directions are pinned by tests that assert
the criteria hit the right ENDPOINT and that the wrong-level field (`adGroup` on Search,
`campaign` on Demand Gen) is empty.

**An unmapped code is REFUSED, not dropped**, and this is the asymmetry worth remembering.
Dropping `"USA"` — a plausible typo for `"US"` — would create a campaign with no criteria that
spends worldwide and reports success, which is precisely the defect being fixed. Validation runs in
the preflight, before the budget mutate, so a typo fails while nothing paid exists. This is the
opposite of meta's default-to-`US`; that is safe there only because Meta's criteria attach during
creation rather than after it. `ValidateCampaignInput` delegates to the same preflight, so the
adoption path cannot accept an input the create path would refuse.

Empty geo stays a no-op — every caller predating the field omits it, and refusing them would break
dispatches that work today. The dispatcher logs a WARN instead, so an untargeted create is findable
in the logs rather than inferred from a Google Ads bill.

**The geo constants are transcribed, not derived.** `US`→2840 and `GB`→2826 have no arithmetic
relation to their codes, so a wrong entry targets the wrong country while looking perfectly valid.
The map is ported verbatim from the legacy Express `GEO_TARGET_MAP`, which is what serves this
channel today, and the happy-path test asserts each code resolves to ITS OWN constant — a map that
returned 2840 for everything would satisfy a "resolves without error" assertion while pointing
every campaign at the United States.

**Note on mutation testing, which produced one false negative.** Five guards were reverted one at a
time. Four failed a test immediately. The fifth — the adGroupCriterion-reports-a-different-ad-group
check — appeared to SURVIVE, which would have meant the test proved nothing. It had not survived:
the restore `cp` and the `go test` were in the same chained Bash command, so the file was restored
*before* the test ran and the test was executing unmutated code. Re-run with the mutation applied
in its own call, it failed. Two lessons, both already in the log but worth restating: verify the
mutation LANDED (`grep -c` the removed text) before believing a survival, and never put the restore
in the same command as the assertion.

That investigation also exposed a real weakness. The test asserted only `err != nil`, and several
unrelated guards in that path also return non-nil — so it would have gone green with the guard
deleted, had the deletion reached it. It now asserts the specific message
(`reports a different ad group id`). A right conclusion resting on a false proof is still a false
proof.

**Docs** — `docs/api-catalog.md` gains `geoTargets` (the contract changed), and
`docs/knowledge/code/internal-platform-googleads.md` gains a "Geo targeting" section plus a scope
line. The frontmatter description was updated too: it enumerates the client's capabilities, and an
enumeration that omits a new one is the kind of claim a later reader trusts. The `demandgen.go`
comment block explaining why geo was deliberately absent is gone — it described a state that no
longer exists, and its closing step string ("no geo targeting set") is now conditional rather than
fixed, since it would otherwise be a lie on every targeted create.

**Out of scope, and still open.** The UI does NOT yet send this field. `buildGoogleAdsConfig`
(`lfx-self-serve` `campaign.controller.ts`) never copies the request's `geoTargets` into
`googleAdsConfig`, even though the UI collects `form.countryCode` and already forwards it to
LinkedIn, Reddit and Meta. Until that one-line-shaped change lands in the UI repo, this service can
target geo but nothing asks it to — the WARN log is how that shows up. Filed as the remaining half
of this ticket's scope item 4.

Separately, `docs/api-catalog.md` still says keywords are capped at "at most 20 entries"; the cap
was raised to 60 (`maxKeywords`). Pre-existing and untouched here.

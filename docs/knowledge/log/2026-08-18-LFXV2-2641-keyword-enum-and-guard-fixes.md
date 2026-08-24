# 2026-08-18 — LFXV2-2641 keyword enum normalisation and guard coverage

**Fix** — findings from the pre-PR reviewer sweep on #153, all verified by a compiling revert
before being fixed and after.

**A closed response Enum the runtime could violate.** `design/brief.go` declares
`Enum("ENABLED","PAUSED","REMOVED","UNKNOWN")` on the keyword row's `status` and
`Enum("EXACT","PHRASE","BROAD")` on `match_type`, and the client passed Google's raw value
through. That is reachable — this is an ACCOUNT-WIDE read returning keywords this service never
created, so the create path's restriction to three match types does not bound it, and Google's
own enums carry `UNSPECIFIED`/`UNKNOWN` while an omitted proto field decodes to `""`. The
consequence is worse than a bad string: **Goa emits response validation in the generated CLIENT,
never in the server**, so the service would happily serialise the value and the generated client
would reject the ENTIRE response over one row. Both fields are now normalised, `match_type`
gained the `UNKNOWN` member it lacked, and unrecognised values FOLD onto `UNKNOWN` rather than
dropping the row — the reading this package already applies to
`CampaignRef.AdvertisingChannelType`: a caller must be able to tell "Google said something we
don't handle" from "Google said nothing". Verified with a probe: `UNSPECIFIED` reached the
result struct untouched before the fix.

**An exclusion where an allow-list belongs.** `ad_group_criterion.status != 'REMOVED'` reads as
"only live keywords", but `UNSPECIFIED`, `UNKNOWN` and `""` all survive it and were offered as
actionable rows — the exact thing the godoc claimed the predicate prevented. Now
`IN ('ENABLED','PAUSED')`, matching `campaignRowIdentity`'s positive switch with its loud
default. Enumerate the live states and default-deny.

**Six orchestrator guards no test reached.** The nil-result contract violations, the nil-slice
normalisations and the pre-dispatch unprovisioned check were all deletable with the suite green:
the instrumentation fake returned non-nil results with empty slices, so it never exercised the
nil arms. Two new dispatcher fakes — one returning `(nil, nil)`, one returning non-nil results
carrying nil slices — now bind all six. The mutation path's asymmetry is pinned deliberately:
reads normalise nil to empty, the MUTATION refuses it, because `applied_count` is derived from
the slice length and a normalised nil would report "zero keywords changed" for a call that
returned success.

**Two vocabularies coupled by string coincidence.** `googleads.Dimension*` and
`model.AudienceDimension*` both spell `age`/`gender`/`device`, and the dispatcher copies the
token through with no mapping. Only `age` was asserted anywhere, so changing `gender` or `device`
alone left every package green while the response would have violated the design's
`Enum("age","gender","device")`. A test now asserts all three pairs. (`age` was already bound —
mutating it did fail the dispatch suite, so the reviewer's "all three packages passed" was wrong
about the evidence and right about the gap.)

**A test that pinned the backstop instead of the guard.** The cancelled-context test asserted
only "errored and did not call", which the transport layer satisfies on its own during token
fetch — the pre-request guard was deletable. It now asserts the guard's distinctive promise, that
no keyword was changed.

**Two coverage gaps in the error classifiers.** The `ErrMetricsWindowUnsupported` arm had no test
(reachable only for non-HTTP callers, since the design Enum stops it at the decoder — which is
exactly why nothing pinned it), and `GetGoogleAdsAudience`'s classification was entirely
untested: its whole `classifyInsightsError` call could be deleted with the suite green. The
error-mapping table is now parameterised over BOTH readers, which also pins that the two stay
identical — the stated design intent.

**One cosmetic correction.** `ctrFor`'s doc claimed it was "centralised so every row type
computes it identically" while `GetCampaignMetrics` still computes the same expression inline.
The comment now describes what it actually covers; de-duplicating `metrics.go` is left to a
change that otherwise touches that path.

**Second round — Copilot's suppressed findings.** `unresolved=0` hid four comments inside the
review body; three were real.

**A destructive mutation proceeding on an unprovable tenant.** The account-identity guard
treated an empty recorded customer id as permission to proceed, matching `ToggleStatus` and
`ReadMetrics`. That reading is wrong for this path and the asymmetry is now deliberate: Google
Ads is ONE customer shared across every foundation, ad-group/criterion ids are account-scoped
bare numerics, and `REMOVE` is irreversible — so a legacy row whose tenant was never recorded
could, after a connection re-point, aim a mutate at another project's keyword on a numeric
collision. Reading the wrong numbers is recoverable; deleting the wrong keyword is not. The path
now fails closed with `ErrCampaignProvenanceUnknown`, checked ABOVE the mismatch arm so the
operator is told to re-dispatch rather than to "reconnect the original account" — an account
that was never recorded and cannot be reconnected to.

**A short outcome slice was a representable partial success.** The orchestrator guarded only
`outcomes == nil`, so a dispatcher returning one outcome for a two-action batch produced a 200
with `applied_count: 1` — precisely the "which half took effect?" ambiguity the atomic batch
exists to remove, and a direct contradiction of the published contract. The check is now on the
COUNT; nil is just its `len()==0` case. The test that previously asserted the partial result now
asserts the refusal, and `applied_count`'s derivation is pinned at the handler instead by a fake
that reports different criterion ids than were requested — the only way to tell "rendered from
the outcomes" from "echoed the request" once the counts must agree.

**A generated CLI example that fails validation.** Goa fabricates an array example by repeating
the element example, so the published `apply-keyword-actions` sample named the SAME criterion
twice — a batch `ValidateKeywordActions` rejects with a 400. Anyone pasting the documented
example got an error. The design now carries an explicit single-action example.

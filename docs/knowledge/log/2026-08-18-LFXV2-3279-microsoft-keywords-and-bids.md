# 2026-08-18 — LFXV2-3279 Microsoft keywords and ad-group bid

**Update** — a Microsoft Search campaign created by this service now carries keywords and an
ad-group bid, so it can actually serve once a human enables it. Before this, MS-2.5 produced a
Campaign → AdGroup → ResponsiveSearchAd tree with **zero keywords**: structurally complete and
commercially inert, because a Search ad group with no keywords has nothing to match a query
against. Enabling it in the Bing UI would have spent nothing and served nothing.

**The endpoint is not the one the sibling suggests, and this is the finding that mattered most.**
Reasoning by analogy to `googleads/targeting.go` (`adGroupCriteria:mutate`) produces a create
that fails every time. Keywords are their own v13 resource — `POST CampaignManagement/v13/Keywords`,
the historical `AddKeywords`. The `AdGroupCriterions` endpoint does exist and is spelled
"Criterions", but its `AdGroupCriterionType` enum has **no `Keyword` member** (Age, Audience, …
Webpage), so keywords cannot be routed through it at all. The first draft of this change did
route them there, with a nested `BiddableAdGroupCriterion` → `Criterion` → `Keyword` body; it
was rewritten against the documented contract before any test was written. `AdGroupId` is a
**sibling** of the `Keywords` array (not a per-keyword field), a Keyword is **flat** with no
`Type` discriminator, and the response is `KeywordIds` + a **flat** `PartialErrors`.

**Keywords are created PAUSED, diverging from the google-ads sibling deliberately.** That client
creates criteria ENABLED, arguing the paused ancestors are gate enough. The argument is
internally consistent but it makes the keyword list the one part of the tree nobody has to
review: it starts spending the moment the campaign and ad group are enabled, which is the
documented next step. The cost of PAUSED is one bulk-enable on a list an operator should be
reading anyway; the cost of ENABLED is unreviewed spend on terms a brief generator produced.
This is a divergence from a sibling, not an oversight in it — recorded here so a later "make
the platforms consistent" pass does not silently flip it back.

**Because they are created Paused, they had to join the status cascade.** Otherwise ACTIVATE
would enable the campaign, ad group and ad while every keyword stayed Paused — a campaign
reporting Active that serves nothing, the exact lie the cascade exists to prevent. Keywords go
before the campaign gate on activate and last on pause.

**No invented default bid.** `AdGroup.CpcBid` is sent only when the caller supplies one, as a
POINTER so an unset bid is OMITTED rather than serialized as `{"Amount":0}`. Microsoft documents
that an ad group with no bid "will be set to the minimum depending on your account's currency" —
a documented, serve-capable floor. An earlier draft hardcoded `defaultCpcBid = 1.0` with a
comment admitting the inheritance behaviour was unverified; the documentation says otherwise, so
the constant was removed. A single hardcoded number could not be currency-correct across every
account anyway, and the floor beats a guess. A REUSED ad group keeps its existing bid: silently
re-bidding a group a human configured would change what a live campaign pays on a retry.

**Geo targeting is NOT in this change, and that is a constraint rather than a deferral.**
Location criteria are campaign-level (`POST /CampaignCriterions` + `LocationCriterion`), and
`LocationId` — a numeric Microsoft identifier — is the ONLY Add-writable element; `DisplayName`
and `LocationType` are read-only, so a country cannot be named. The v13 API accepts an ISO 3166
code for targeting **nowhere**; the ISO table in Microsoft's own Geographical Location Codes
guide is scoped to account business ADDRESSES, which is an easy misread, and the locations file
has no ISO column. The sibling dispatchers' `geoTargets` are ISO-2 strings, so honouring them
here would mean hardcoding an invented ISO→LocationId map on the path that spends money. So
`microsoftConfig` carries **no** `geoTargets` field: offering one the dispatcher would silently
drop is worse than not offering it. Note the ticket asked for geo; this is the part it asked for
that the API does not support as specified.

**ToggleStatus refuses to ACTIVATE a campaign with no provisioned keywords**
(`ErrCampaignNotProvisioned` → 409, raised locally without calling Microsoft), mirroring the
google-ads keyword-criteria gate. PAUSE requires none — refusing it would strand a campaign an
operator is trying to stop.

**Tests** — the request-body tests use three keywords with three DIFFERENT texts AND three
DIFFERENT match types, asserting each pair together, so a swap between two keywords or between a
text and a match type fails. A fixture with a repeated match type would have stayed green
through exactly that bug. The unset-bid test asserts against the RAW body rather than the decoded
struct, because a decoded zero cannot distinguish "omitted" from "explicitly zero" — which is the
bug being guarded.

**Mutation-verified**, nine guards, each reverted and confirmed to fail a test: short id array →
UNCONFIRMED; PartialError classified before the id count; unset CpcBid omitted; no 429 retry on
the keyword create; the ACTIVATE keyword guard; keyword ordering before the campaign gate;
case-insensitive dedupe; control-character rejection; and up-front validation before any create.
The ACTIVATE keyword guard initially failed this check — removing it broke nothing, because the
existing toggle tests all predated keywords — so
`TestMicrosoft_ToggleStatus_ActivateWithoutKeywordsIsNotProvisioned` was added, with a fixture
carrying a COMPLETE ad group and ad so it cannot pass by accident on the missing-child guard.

**Contract** — no `design/` or `gen/` change: per-platform `config` is `Any` in the Goa design, a
free-form JSON envelope, so `keywords`/`cpcBid` are additive. `docs/api-catalog.md` is updated.

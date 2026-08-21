# 2026-08-19 — Meta: reject a malformed IGSID and a one-sided DSA pair pre-create

**Fix** — The Instagram identity and EU DSA disclosure fields were trimmed in
`CreateCampaign` but never validated, so two deterministic input faults were only
discovered AFTER a billable campaign existed. `CreateCampaign` states the governing
rule in its own comment — resolve the objective and validate deterministic inputs
BEFORE the first mutating call, so an input error never creates a paid campaign — and
both fields sat outside it. Both guards now run immediately after the trim block,
alongside the placement and promoted-object validations.

**A malformed `InstagramUserID` reached the creative POST.** The field is only
consumed there, which runs after the campaign and ad set already exist, and where a 4xx
is treated as a NON-FATAL per-variant failure. The outcome was a `created_degraded`
campaign with no publishable ad: a billable resource that can never serve. Meta object
ids are decimal strings and `numericIDRE` already gated `PageID` and `PixelID` for
exactly this reason, so a supplied value now gets the same gate. An EMPTY value is
still accepted — that is a Facebook-only campaign, and the field stays optional.

**A one-sided DSA pair was accepted.** `dsa_beneficiary` and `dsa_payor` have
INDEPENDENT attach guards, so exactly one could be sent. Meta requires BOTH to publish
a regulated ad set, so one is deterministically incomplete: it either gets the ad set
rejected after the campaign exists, or leaves it unpublishable. Both-absent remains
valid — that is the non-regulated flow. A whitespace-only counterpart trims to absent
and therefore still counts as one-sided.

This REVERSES the "no hard validation was added" note in
[`2026-08-19-meta-instagram-dsa-publishability.md`](2026-08-19-meta-instagram-dsa-publishability.md),
and narrowly. That entry rejected a *presence* gate, which would have fired on the
default-placement path and broken unrelated flows; that reasoning still holds and
presence is still NOT validated, because whether a disclosure is required at all
depends on the targeted location, which this client does not evaluate. What changed is
the narrower class: a malformed IGSID and a one-sided pair are unpublishable regardless
of targeting and knowable before any upstream call, and this repo's rule is that a
permanent input fault must fail before a billable resource exists.

Both rejections use the file's existing shape for a deterministic input fault — a bare
`fmt.Errorf` returned with a `nil` result, no sentinel, matching the `AccountID` /
`PageID` / budget guards. The `nil` result is load-bearing: the dispatch adapter
releases the claim ONLY when `result == nil`, wrapping that case in `notCreated(...)`
→ `NoUpstreamCreate()` → the orchestrator RELEASES the dispatch claim, which is correct
because nothing was created upstream.

Tests: `internal/platform/meta/client_test.go` gains
`TestCreateCampaignRejectsMalformedInstagramUserIDBeforeAnyPost`,
`TestCreateCampaignAcceptsValidAndAbsentInstagramUserID`,
`TestCreateCampaignRejectsOneSidedDSAPairBeforeAnyPost` and
`TestCreateCampaignAcceptsBothOrNeitherDSADisclosure`. The rejection paths run against
`noPostServer`, which FAILS the test on any POST — so they assert the rejection happens
before any upstream call, not merely that an error is returned. A test that only
checked for an error would still pass if the guard moved after the campaign create.
Each guard was mutation-verified with a compiling revert; every mutation was caught,
including a transposition of the DSA roles in the error message.
`TestCreateCampaignOmitsInstagramAndDSAWhenUnset` passes unchanged — its whitespace-only
values trim to empty, which is both-absent and valid. The api-catalog wording, which
said these were "NOT validated locally", was narrowed to state exactly what is now
validated (format; one-sidedness) and what still is not (presence).

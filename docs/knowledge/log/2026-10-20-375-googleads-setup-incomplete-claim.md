# 2026-10-20 — google-ads: a campaign partial does not prove the setup stopped short

**Fix** — [#375](https://github.com/linuxfoundation/lfx-v2-campaign-service/issues/375). The
`campaign_created_setup_incomplete` sentence told the operator the service
*"created the campaign but did not finish setting it up; it exists in a partly-configured state
and is not ready to run"*. The token's evidence does not reach that far.

The cascade's final mutate can answer **2xx with a response this client cannot read** —
`pmax_creative.go`'s `assetGroupAssets:mutate` and `demandgen_creative.go`'s
`adGroupAds:mutate` both return a campaign partial alongside an error whose own text says the
resources MAY exist. In that case the step committed and the setup *did* finish; only the
confirmation was lost. The returned campaign id proves the campaign exists. Nothing in the
partial proves its setup stopped short.

The sentence now says the service *"could not confirm its setup finished"*. The recovery
instruction — verify on the platform before retrying — was right either way; the assertion in
front of it was not. `domain.CreateOutcomeCampaignCreatedSetupIncomplete`'s doc comment and
`TestDispatchFailureMessage_OutcomeArms`'s pinned substrings move with it, since the test pins
the CLAIM rather than the phrasing and the claim is what changed.

The token name is deliberately unchanged: it is a wire value carried by
`internal/dispatch`'s `withCreateOutcome`, and renaming it would be a contract change for a
wording correction.

Same shape as [2026-10-20-374-googleads-codeless-4xx-refusal-claim](2026-10-20-374-googleads-codeless-4xx-refusal-claim.md)
— a definite claim resting on evidence that was never gathered. Original classification work:
[2026-10-20-googleads-dispatch-failure-classification](2026-10-20-googleads-dispatch-failure-classification.md).

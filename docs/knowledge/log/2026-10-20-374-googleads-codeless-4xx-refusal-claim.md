# 2026-10-20 — google-ads: a codeless 4xx is not evidence of a campaign refusal

**Fix** — [#374](https://github.com/linuxfoundation/lfx-v2-campaign-service/issues/374). The
budget-created arm of `googleads.CreateOutcome` claimed
`budget_created_campaign_refused` on any definite 4xx `*apiError`, without requiring that the
response's error codes had parsed. `doRequest` builds an `apiError` with `ErrorCodes` nil
whenever a non-2xx body is unreadable or over `maxResponseBytes` (`client.go`), so a
`DUPLICATE_CAMPAIGN_NAME` refusal arriving that way slips past
`isDuplicateCampaignNameErr` — the arm that exists to catch it — and lands on the sentence
promising that *no campaign of the composed name exists* and the budget should be removed
before retrying. An earlier attempt's campaign is standing at that moment, so the operator
deletes a live campaign's budget and retries into a second billable campaign.

The gate now also requires `len(ae.ErrorCodes) > 0`. The duplicate arm is what makes the
refusal sentence true, and it can only do its job on codes it could read; where it could not
read them, the weaker `budget_created_campaign_unconfirmed` is the strongest claim the evidence
supports. The known trade-off is accepted deliberately: a genuine 4xx carrying no
`GoogleAdsFailure` detail now weakens to "unconfirmed", which is a true sentence that costs the
operator one verification, rather than a false one that costs a duplicated campaign.

**Why a green suite missed it.** `TestCreateOutcome_OversizeErrorBodyDegradesToATrueClaim`,
written with the original change, fed a codeless 4xx to the **id-less** partial only, never to
the budget partial. The arms are separate and so are their guarantees: *one call site's test
does not pin another's.* The table now covers the budget arm with the same shape.

This is the same mistake as the two corrected before the PR merged — a definite claim resting
on evidence that was **missing** rather than **present** — made once more inside the fix for it.
See [2026-10-20-googleads-dispatch-failure-classification](2026-10-20-googleads-dispatch-failure-classification.md)
for the original classification work, and
[2026-10-20-375-googleads-setup-incomplete-claim](2026-10-20-375-googleads-setup-incomplete-claim.md)
for the sibling defect in the rendered sentence.

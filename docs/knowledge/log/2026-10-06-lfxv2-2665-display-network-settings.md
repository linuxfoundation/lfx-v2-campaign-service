# 2026-10-06 — LFXV2-2665 Display campaigns send networkSettings

**Fix** — The one shipping defect found by the local pre-PR review of this branch: every
Display create omitted `networkSettings`, and every Display create would therefore have
stranded a billable budget.

**An omitted `networkSettings` is not a default, it is an error raised too late.** The
flags are proto3 bools, so a campaign create without the field resolves to a campaign
targeting NO network. Google rejects that with
`CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK` — but only at `campaigns:mutate`,
which this cascade reaches AFTER the budget mutate has committed. The budget is billable
and orphaned, and the failure does not self-heal: a retry composes the same budget name and
dies at `DUPLICATE_NAME`. This is precisely the orphan the preflight-then-create ordering
exists to prevent, which is why the field is a correctness bug and not a settings nit.

**The consistent-looking inference was the wrong one, and so was adding the field blindly.**
Demand Gen, Performance Max and Video all REJECT `networkSettings` — their network is
implied by the channel — and each has a test asserting its absence. From there, "Display is
not Search, so omit it like the other three" is the reading that looks like it respects the
pattern. It does not: Display is the one non-Search channel that takes the field. The
matrix is two of five, SEARCH and DISPLAY, and it is now written down in
`internal-platform-googleads.md` and on the `networkSettings` type itself, because the
shape of the mistake is that each half is individually plausible.

**How the flags were settled without a mutate.** A `validateOnly` mutate is still a POST to
a live production account, so it was not available. The `Campaign` RPC reference and the
`create-campaigns` guide are both inconclusive on which flag a DISPLAY campaign sets; the
Google Ads API team's own answer is explicit — `targetContentNetwork` true, the others
false. That matches the thing the name already says: the Google Display Network *is* the
content network. `targetGoogleSearch` and `targetSearchNetwork` are sent explicitly false
rather than omitted, so the payload states its intent and a Display campaign cannot drift
into Search inventory.

**The test asserts the flags, not the field.** `TestCreateDisplayCampaign_HappyPath` reads
`networkSettings` out of the captured request body and checks all three booleans by name. A
presence-only assertion would stay green through a later edit that flipped
`targetContentNetwork` to false — which is the same campaign-targets-no-network failure
with the field technically present.

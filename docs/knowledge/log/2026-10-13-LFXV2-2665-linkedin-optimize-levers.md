# 2026-10-13 — LFXV2-2665 LinkedIn settings readback, bid lever, adoption and campaign-ref

**Update** — LinkedIn gains the four capabilities every other non-Google ad platform already had
(LinkedIn moved into scope 2026-10-13). `LinkedInDispatcher` now implements `SettingsReader`
(`linkedin_settings.go`), `BidWriter` (`linkedin_bid.go`) and `CampaignAdopter`
(`linkedin_adopt.go`), all reading the campaign through one identity-checked
`linkedin.Client.GetCampaignSettings` (`campaign_settings.go`), whose 404 is absent only after a
confirming ad-account read. The bid lever writes the campaign's `unitCost` only under costType CPC
with optimizationTargetType NONE/ENHANCED_CONVERSION, sent once (`withSendOnce`), REJECTED (400)
only on LinkedIn's parsed, identity-checked error envelope naming `/Campaign/unitCost/amount`.
Finding: the create path sends no optimizationTargetType, which LinkedIn's schema defaults to
NONE (manual), so created campaigns are expected to pass — the guard still decides on the reported
value. Adopted rows carry `adopted: true`: ACTIVATE and the bid lever are refused, PAUSE flips the
campaign alone, the budget lever works. New route `resolve-linkedin-ads-campaign`
(`GET /projects/{project_id}/linkedin-ads/campaign-ref`) with HTTPRoute regex, RuleSet and parity
rows. See [internal/dispatch](../code/internal-dispatch.md) and
[internal/platform/linkedin](../code/internal-platform-linkedin.md).

PR #318 review: adoption now refuses a campaign whose `account` LinkedIn did not report (409,
`ErrCampaignAccountMismatch`), matching the bid lever, instead of trusting the path scope alone;
and `ValidateBidAmount` checks "rounds to zero" BEFORE the whole-cent check — the latter's
absolute tolerance would otherwise pass an amount under 1e-8 as a "0.00" wire.

Second #318 review round: `parseDecimalMicros` refuses an amount past int64 micro-units instead of
wrapping it negative; a budget object present without an amount marks the budget unreadable;
adoption validates the echoed account BEFORE interpreting a terminal status; the bid lever refuses
an unreported status; `ValidateBidAmount` requires the two-decimal wire form to equal the requested
float exactly (no tolerance); and the bid endpoint description scopes "not USD" to the non-LinkedIn
adapters.

Third #318 review round: the bid rejection classifier now REQUIRES every input error to echo
`input.value.decimalValue` equal to the submitted bid; an envelope that omits it is no longer an
amount REJECTED (400) and stays an ordinary definite failure.

Fourth #318 review round: the LinkedIn settings readback reports `unit_cost_currency`
(`unitCost.currencyCode`) beside `unit_cost`, and leaves the amount absent when the currency is
missing or empty, so a EUR bid never reads as a bare number. The bid write already refuses (409) a
campaign whose unitCost is not USD or reports no currency, so it never writes USD over another
currency.

Fifth #318 review round: `linkedin.ValidateCampaignID` and `ValidAccountID` now require a positive
int64 (shape plus `ParseInt`, as Microsoft's rule does), so `9223372036854775808` is refused by
adoption, campaign-ref (400 before the lookup), the settings readback and the bid lever; a
connection storing such an account id is an unusable connection (409) on the readback and the bid
lever. The design and the decoder-drift test now state that the Pattern is the shape only.

Sixth #318 review round: the bid lever and the settings readback validate the stored
`platform_campaign_id` verbatim (a padded " 777" fails closed — 409 — instead of reading or
mutating campaign 777; adoption and campaign-ref already did). An adopted LinkedIn bid refusal now
carries the new `domain.ErrBidAdoptedCampaign` alongside `ErrBidUnwritable`, and the bid handler
answers it with its own 409 naming adoption and Campaign Manager instead of the generic
strategy/ad-group text.

Seventh #318 review round: the `resolve-linkedin-ads-campaign` 400 now states the enforced bound
("a positive integer no greater than 9223372036854775807 … without a leading zero") instead of
"at most 19 digits", which a 19-digit id past int64 also satisfied. The other LinkedIn-id refusals
already state the 64-bit bound or are platform-neutral (adoption's 400).

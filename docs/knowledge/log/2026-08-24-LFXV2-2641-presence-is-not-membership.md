# 2026-08-24 — presence is not membership

**Fix** — Google Ads keywords/audiences: the two project-scoped reads sent a
`campaign.id IN (...)` predicate and then trusted that it had been honoured. The response loop
checked only that each row's `campaign.id` was NON-EMPTY. A row naming a campaign outside the
requested set was returned to the caller as a successful read, and Google Ads is ONE customer
shared across every foundation, so that row is another project's keyword text, spend and
demographics.

Reproduced by construction before anything was changed: a stub answering a request scoped to
`[555]` with a row for campaign `999` yielded
`CampaignID:999 Text:"rival secret keyword" CostMicros:25000000` in `KeywordPerformance.Rows`.
The same probe against `GetAudienceInsights` returned a bucket carrying 5000 impressions and
77000000 micros from campaign 999.

## Why the previous guard read as sufficient

The guard it replaced was itself a fix, landed the same day, and its stated reasoning is sound:
an absent id means the SELECT and the decode struct have drifted apart, and `campaign_id` is
`Required()` on the design type, so a row carrying `""` should not be reported as success. All
of that is still true. But it answers "does this row name a campaign", and the question the
tenancy boundary asks is "does it name one of the campaigns asked for". Those look alike at the
call site — both are a check on `row.Campaign.ID` — and only one of them is a boundary. A fix
that makes a field non-empty can leave the property the field exists to prove entirely unchecked.

## The shape of the fix

`campaignScopePredicate` now returns the canonical scope SET with the predicate string, built in
ONE canonicalisation pass so the filter sent and the membership enforced cannot drift apart.
Both readers call `assertCampaignInScope` per row. It errors on the whole response rather than
skipping the offending row: skipping reduces an unhonoured query to "this project has little
data", the clean partial answer a caller totals and acts on. `campaign_lookup.go` already
states this rule for its own id filter; reusing the reasoning is what keeps the surfaces aligned.

Ids are canonicalised on both sides, so `0555` cannot read as a foreign campaign and a
non-canonical spelling of an in-scope id cannot pass by string equality.

## The sibling was the more dangerous one

`GetAudienceInsights` had the same defect and could not even check: it never SELECTed
`campaign.id`. Its buckets never report a campaign, so a foreign campaign's numbers are summed
into a bucket and become indistinguishable from the project's own — a silent wrong total rather
than a visibly foreign row. It now selects the column solely to make the check possible, and
checks BEFORE aggregating. `resolveKeywordCriteria`, the third scoped read, was already correct:
it re-checks each (ad group, criterion) pair against the requested set rather than trusting the
IN/IN filter.

## A test that asserted the filter while claiming to test the projection

Dropping `campaign.id` from the audience SELECT was a COMPILING mutation that every audience
test survived, including the one written to catch it. The stub server answers whatever fixture
the test wrote regardless of what the query asked for, so nothing bound the projection. The
first attempt asserted `strings.Contains(query, "campaign.id")` — which passes on the scope
predicate's own `campaign.id IN (...)` in the WHERE clause, matching the filter it was not
testing. The assertion now slices the SELECT list out of the query before checking it, and the
mutation dies.

The general form: when a value appears in more than one clause of a generated string, a
substring assertion over the whole string does not pin the clause you mean.

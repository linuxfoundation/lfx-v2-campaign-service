# 2026-08-19 — LFXV2-2641 the comments outlived the boundary they described

**Docs** — the keyword and audience reads were narrowed from the shared Google Ads customer to
the calling project's own campaigns, and `design/` was corrected earlier tonight. Nine Go
comments, one API-catalog sentence and one knowledge paragraph still published the WIDER
boundary.

## A stale comment about a security boundary is not a cosmetic lag

Google Ads is ONE customer shared across every foundation, so a read scoped only by the
connection returns every project's keyword text, spend and demographic distribution.
`campaignScopePredicate` narrows both reads in `internal/platform/googleads/keywords.go` to the
`platform_campaign_id`s this service holds for the project. That is the tenant boundary, and it
is enforced.

What was still being *said* about it was wider than what was *done*. Three consequences, all
reachable:

- **A consumer mis-presents the data.** `AudienceInsights` was documented as "an account-wide
  demographic read across every breakdown". A caller believing that renders another
  foundation's age/gender/device split under this project's name.
- **A future author drops the scope.** A comment that reads `campaignIDs` as a filter rather
  than as the thing standing between one project and everyone else's spend invites its removal
  as an optimisation. `campaignScopePredicate` refuses an empty list precisely because that
  mistake is one dropped guard away.
- **A test's stated reason becomes false.** `keywords_test.go:671` justified enum normalisation
  with "An ACCOUNT-WIDE read returns keywords this service never created". The conclusion
  survives — an ADOPTED campaign, or one edited in the Google UI, reaches the same place — but
  the premise does not, so the test stood on a proof that no longer held.

## Every site the class sweep found

Swept the repo for `account-wide`, `the account's`, `shared customer`, `whole account` and
`entire account` on any read path now campaign-scoped. Eleven sites changed:

| site | claim |
| --- | --- |
| `internal/domain/model/connection.go:292` | `KeywordPerformance` "is an account-wide keyword read" |
| `internal/domain/model/connection.go:297` | `Truncated`: "must not total ... as account-wide spend" |
| `internal/domain/model/connection.go:319` | `AudienceInsights` "is an account-wide demographic read" |
| `internal/platform/googleads/keywords.go:247` | `GetKeywordPerformance` "reads the account's top keywords" |
| `internal/platform/googleads/keywords.go:267` | "the two account-wide reads in this file" |
| `internal/platform/googleads/keywords.go:415` | `GetAudienceInsights` "reads ... for the account" |
| `internal/platform/googleads/keywords_test.go:88` | "would total the slice as the whole account's spend" |
| `internal/platform/googleads/keywords_test.go:113` | "a partial slice when they are the whole account" |
| `internal/platform/googleads/keywords_test.go:302` | "report a single campaign's numbers as the whole account's" |
| `internal/platform/googleads/keywords_test.go:671` | "An ACCOUNT-WIDE read returns keywords this service never created" |
| `internal/infrastructure/postgres/campaign_repo_test.go:123` | "authorizes the account-wide Google Ads keyword/audience endpoints" |

Plus two prose surfaces: `docs/api-catalog.md:115`, whose row is emphatic that the read is
project-scoped and then closed by contrasting the cap with "the account's full set"; and
`docs/knowledge/code/internal-platform-googleads.md:901`, "would report a single campaign's
numbers as the account's".

## What the sweep deliberately did NOT change

The same phrases are load-bearing and TRUE in a second construction: these reads *would be*
account-wide without the predicate. Every guard in the chain is written that way on purpose —
`campaignScopePredicate`'s empty-list refusal ("an unscoped read would return every project's
data"), the orchestrator's empty-scope early return, the dispatcher's post-filter check
(`internal/dispatch/googleads.go:1162`, "would put the account-wide read one dropped guard
away"), and `keyword_actions_test.go`'s scope assertions. Rewriting those would delete the
reason the guards exist.

`ProjectCampaignScope` (`internal/domain/model/campaign.go:438`) and `brief_port.go:130` are
already exact — "the authorization scope for an otherwise account-wide platform read" — and
were left alone. So were the many unrelated hits: `the account's currency` across the Meta,
Microsoft and Reddit clients is monetary denomination, and the LinkedIn and Microsoft
campaign-name lookups genuinely ARE account-wide searches.

`docs/plans/keyword-surface.md` describes the pre-existing BFF behaviour this work replaced and
is historical; it is not a claim about current code.

**The rule this leaves behind:** whoever narrows a read adopts every claim about it they leave
standing. The claim to delete is that the RESULT is account-wide; the claim to keep is that it
would be, absent the predicate.

## No behaviour change, so nothing to mutate

Comment- and prose-only. What was verified instead is that the new claims are TRUE:
`campaignScopePredicate` is called at `keywords.go:317` and `:447`, and the audience read
computes `scope` once before the per-dimension loop and interpolates it at `:459`, so all three
GAQL queries carry it or the call fails. `make test`, `go vet`, `gofmt -s` and golangci-lint are
green.

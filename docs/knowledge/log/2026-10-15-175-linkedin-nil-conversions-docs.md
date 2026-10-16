# 2026-10-15 — Nil conversions documented as "not measured", and no_conversions' click floor

**Docs** — Issue #175, the decision-free part. No behaviour change.

- **Nil conversions is "not measured".** The contract text described a nil
  `CampaignMetrics.Conversions` as "this channel does not report a campaign-level conversion
  count", but LinkedIn withdraws the whole total to nil when any returned element omits
  `externalWebsiteConversions`, so an incomplete LinkedIn read looked like an incapable platform.
  The `conversions` attribute in `design/brief.go` (regenerated with `make apigen`) and the
  Conversions section of `code/internal-dispatch.md` now define nil as "not measured — the
  platform cannot report it, or (LinkedIn, Microsoft) the read was incomplete", matching how
  Microsoft's blank-cell withdrawal was already documented and what `model.CampaignMetrics`
  already said. Comments in `rules.Input.Conversions` and LinkedIn's empty-`elements` branch,
  and `code/internal-platform-linkedin.md`, now say the same.
- **`no_conversions` never fires on a zero-click window.** `code/internal-dispatch.md` called a
  quiet Google campaign "precisely the campaign the rule exists to catch", but the rule requires
  `Clicks >= minClicksForConversions` (50). The sentence now says the no-rows zero is about the
  truth of the field, and that a campaign with no delivery is `zero_delivery`'s finding.
  `code/internal-service-rules.md` says the same in the rule table and the floors section.
- `docs/api-catalog.md`'s metrics-endpoint entry carries the same definition.
- Test: `TestGetCampaignMetrics_NilConversionsIsAnIncompleteRead` is a table-form pin of the
  current LinkedIn outcomes over multi-element responses (one element omits the field → nil;
  every element carries it → the sum; every element omits it → nil), which the existing
  single-case tests cover one at a time; it also checks impressions and clicks still aggregate
  when conversions are withdrawn.
- Not in scope: a `conversions_status` enum to tell the two nil cases apart on the wire is a
  separate follow-up on #175. Googleads comments and tests (including
  `internal/dispatch/googleads_test.go`) that still say nil is reserved for platforms that cannot
  report at all were left untouched.
- Concepts updated: `code/internal-dispatch.md`, `code/internal-service-rules.md`,
  `code/internal-platform-linkedin.md`.

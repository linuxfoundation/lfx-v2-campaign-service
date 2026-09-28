# 2026-09-28 — A2: Reddit reported zero conversions it had never measured

**Fix** — behaviour change, operator-facing and response-shape, Reddit only. Refs
`linuxfoundation/lfx-self-serve#3020`.

The Reddit account-monitor read asks Reddit for `IMPRESSIONS`, `CLICKS` and `SPEND`. It has
never asked for conversions. The dispatcher nevertheless set every row's `Conversions` to a
non-nil `0`, copying the BFF's `campaignMetrics[].conversions = 0` — a measurement claim with
nothing behind it.

That fabricated zero had a consumer. `redditActionItems`' "clicks but 0 conversions" rule reads
`Conversions != nil && *Conversions == 0`, so it fired for **every** Reddit campaign past its
100-click floor, on every response, advising the operator to check their pixel and landing page.
It could not be satisfied any other way: a real nonzero count had no path to that code.

`Conversions` is now left nil. The rule goes dormant rather than being deleted — it is correct
as written, and it lights up on its own the day a real conversions read lands.

Nothing about this required a decision, only noticing. Three places already said what the right
answer was:

- `AccountMonitorCampaign`'s design-layer comment: *"ABSENT when this platform/row could not
  measure conversions — not a measured 0."* The field is already Optional; the response for a
  Reddit row now omits it instead of carrying a 0.
- `model.AccountCampaignMetrics.Conversions`: nil means could-not-measure, a non-nil 0 is a
  measurement.
- `internal/platform/reddit/metrics.go`, the sibling reader on the single-campaign brief path,
  which leaves `Conversions` nil for exactly this reason and explains it: a guessed field that
  decodes to zero is indistinguishable from a campaign that converted nothing.

So the monitor path was contradicting its own package, and the contract it violated was written
on the field it was setting.

## Why no test caught it

All four existing `TestReddit_ListAccountCampaignMetrics_*` tests are refusal tests — malformed
account id, invalid days, system fallback, mismatched account. Every one asserts that a bad
request is rejected. **Not one exercised a successful read**, so nothing pinned what the mapping
actually produces, and a fabricated field in it had nowhere to be noticed.

`TestReddit_ListAccountCampaignMetrics_ConversionsAbsentNotZero` closes that: it stands up a
fixture account and asserts the mapping, conversions included. Verified rather than reasoned —
run against the parent commit in a throwaway worktree it fails with
`Conversions = 0, want nil`. Its report fixture carries a plausible-looking `conversions` key
deliberately, so the test also fails if a later edit starts opportunistically reading a guessed
field name; `TestReddit_ReadMetrics_ConversionsAbsentNotZero` uses the same device on the brief
path.

`TestRedditClicksNoConversions_DormantWhileUnmeasured` replaces the rules-side test that had
pinned the always-firing behaviour as intended, and its second subtest supplies the measurement
the dispatcher does not yet have, so the rule's own logic stays covered while it waits.

# 2026-08-08 — invalid UTF-8 in a lookup name is a silent query rewrite

**Update** — `gaqlStringLiteral` now rejects invalid UTF-8, because `encoding/json` would
substitute U+FFFD for the malformed bytes without error and `FindCampaignByName` would then
query a name the caller never passed.

The three guards already in front of the query all pass a malformed byte through:

| guard | what it sees | verdict |
|---|---|---|
| `utf8.RuneCountInString` length check | one rune | passes |
| `for _, r := range s` NUL/LF/CR check | `utf8.RuneError` (U+FFFD) | passes |
| row-level `campaign.name` re-check | nothing — zero rows to check | never runs |

Proven with a throwaway program: `json.Marshal` of a struct holding `"camp\xffaign"` returns
`err=<nil>` and `{"query":"...'camp\ufffdaign'..."}`. Reverting the new guard reproduces the
defect exactly — the test server holding a campaign named `bad\ufffdname` receives
`WHERE campaign.name = 'bad\ufffdname'`, and the lookup returns `("", nil)`, the licence to
create a duplicate paid campaign.

The rejection is safe by the same test the NUL/LF/CR rejection passes: an invalid-UTF-8 name
could never have been stored, because Google Ads' JSON and proto surfaces both require valid
UTF-8. So no reachable lookup is refused — which is the only thing over-rejection ever costs
here, and the reason the blanket `unicode.IsControl` rule was removed earlier.

The lesson generalises past GAQL: **a fail-closed lookup must validate the value that will be
transmitted, not the value it was handed.** Any encoder that repairs its input lossily and
reports success — `encoding/json` here — sits between the two and can turn a specific question
into a different one, whose miss is indistinguishable from a real absence.

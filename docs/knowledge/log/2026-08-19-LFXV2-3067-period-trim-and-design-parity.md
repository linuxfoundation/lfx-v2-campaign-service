# 2026-08-19 — LFXV2-3067 the period consumer undid the verbatim-decode fix

**Fix** — two follow-ups on the settings-readback review fixes: a consumer that trimmed the
value the decode step had just stopped trimming, and an authored contract still describing a
field as uncomparable after it was wired up to compare.

## The trim moved one layer down instead of going away

The previous commit changed `blankToNil` to stop trimming the values it keeps. Its
doc-comment names the reason precisely, and names both consumers:

    // googleAdsDateOnly parses with a strict layout and, on failure, passes the value
    // through WHOLE — so an upstream "2026-08-01 " trimmed to "2026-08-01" is byte-equal
    // to a recorded YYYY-MM-DD date and reports `match` for a value that never parsed.
    // googleAdsBudgetTypeFromPeriod has the same shape with " DAILY ".

`googleAdsDateOnly` honours that. `googleAdsBudgetTypeFromPeriod` did not — it opened with
`switch strings.TrimSpace(period)`. So `" DAILY "` survived the decode verbatim, exactly as
intended, and was then normalised into `DAILY` by the very function the comment cites as the
thing being protected. Against a row recording a daily budget the verdict came back `match`:

    budget_type Upstream = "daily", comparison = "match"

for a period string Google never sent. Half of the fix stopped normalising; the other half
still did, and the second half is the one that decides the verdict. The comparison now uses
the exact `DAILY` / `CUSTOM_PERIOD` spellings, so a padded value takes the same fail-closed
path as `UNKNOWN` — absent upstream side, `unknown` verdict.

The general shape is worth keeping: **a decode step that stops normalising buys nothing
unless every consumer downstream of it also declines to normalise.** One trim anywhere below
restores the fabricated agreement for that field, and it does so invisibly, because the layer
that was fixed still looks correct in isolation.

The test for it has to run through the real decode path and, more importantly, has to pin a
recorded side of `daily` — the value a trim makes the padded field compare EQUAL to. A test
whose recorded side differed would read `divergent` either way and pass against the bug.
Reverting the fix turns it red:

    budget_type Upstream = "daily", want nil: " DAILY " is not a spelling this API version
    names, and trimming it into DAILY fabricates a value the platform never sent
    budget_type comparison = "match", want unknown for a padded period

## The authored contract still said the field could never be compared

The same previous commit gave `advertising_channel_type` a recorded side from
`ConfigSnapshot` and started comparing it. The `campaign-settings-field` description in
`design/brief.go` was not updated with it, and still listed the field among those

    reported UPSTREAM-ONLY, with no `recorded` counterpart and therefore always an
    `unknown` verdict, because no column on the campaign row expresses them

which is now false in both clauses. That description is not a comment: Goa copies it verbatim
into the generated service and client types and into all four OpenAPI documents, so the stale
text was the contract every consumer reads — telling them a field that can now report a real
misconfiguration will only ever say `unknown`. Moved to the compared list, with a note that it
still reads `unknown` on a snapshot-less legacy row for the ordinary reason (nothing was
recorded), and regenerated: `gen/` plus the `cmd/campaign-service/kodata/gen/http/openapi*`
copies.

`model.CampaignSettingsReadback.UnknownCount`'s doc-comment carried the matching stale claim,
that "seven of the ten are permanently unknown by construction". That number was re-measured
from the code rather than re-reasoned, by running a fully-healthy readback both ways:

    with snapshot:      total=10 unknown=6
    legacy no snapshot: total=10 unknown=7

The floor is a RANGE, six to seven, exactly because this field's recorded side depends on the
snapshot. Any single number is wrong for one of the two rows. That does not weaken the point
the comment is making — it sharpens it: a consumer watching `unknown_count` for read failures
sees a non-zero baseline it cannot distinguish from a real one, and cannot even pin that
baseline to one value.

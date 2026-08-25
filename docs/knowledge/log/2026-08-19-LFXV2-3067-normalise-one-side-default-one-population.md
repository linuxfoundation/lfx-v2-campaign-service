# 2026-08-19 — LFXV2-3067 normalising one side, defaulting one population

**Fix** — two readback fields reported agreement that the data does not support. Both are the
class the previous commit fixed on a sibling field an hour earlier: `campaign_name` trims one
side of its own comparison, and `advertising_channel_type` extends a legacy default to a
population it was never reasoned about.

## Trim to DETECT, never to PRODUCE

`recordedName` was built like this:

    if n := strings.TrimSpace(campaign.CampaignName); n != "" {
        recordedName = strPtr(n)
    }

while `settings.Name` was carried verbatim from the platform. A row holding `" same name "`
therefore compared byte-equal to an upstream `"same name"` and was reported as a **match**. The
two plainly differ, and Google is showing the operator a name this service did not record —
exactly the finding this endpoint exists to surface.

The padded row is reachable, not hypothetical: `UpdateCampaign` assigns
`p.Campaign.CampaignName` verbatim (`internal/service/brief.go:1107`) and there is no
whitespace validation in the service layer, in `design/`, or on the column.

This is the same defect the previous commit fixed on `budget_type`, one field over. That one
was `googleAdsBudgetTypeFromPeriod` calling `TrimSpace` on the UPSTREAM period; this one is the
RECORDED side of a different field. The knowledge entry for it stated the rule as "every
consumer downstream of the decode must be checked for a trim of its own" — correct, and it
stopped at the upstream half. The recorded side is a consumer too.

**`TrimSpace` still has exactly one job here: DETECTING an all-blank legacy value**, which
means the row never captured a name and must read `unknown` rather than diverge against a real
upstream one. Trim to decide whether a value EXISTS; never to build the value you COMPARE.
Deleting the trim outright would swap one fabricated verdict for another — every blank column
would start diverging — which is why `TestGoogleAds_ReadSettings_BlankRecordedNameIsUnknownNotDiverged`
exists alongside the padded test.

**The test has to use padded values.** Trimming a string with no surrounding whitespace is the
identity, so a fixture built from unpadded names passes against the bug — which is precisely
why the existing `MatchWhenBothAgree` and divergence tests, both using `"same name"` and
`"upstream name"`, could never have caught it. Reverting the fix now fails 7 subtests, each
reporting `match` for two values that differ.

### Every field this comparator handles, swept

Ten fields. Five are upstream-only (`nil` recorded), so no comparison can be fabricated:
`status`, `budget_delivery`, `budget_shared`, `bidding_strategy` and — separately — the flight
dates, whose recorded side is always NULL for Google Ads today.

| field | recorded side | verdict |
| --- | --- | --- |
| `budget_amount` | `formatBudgetUnits` | **safe** — both sides through the same numeric formatter; sub-cent micros deliberately rendered at full precision so they compare UNEQUAL |
| `budget_type` | `string(*campaign.BudgetType)` verbatim | **safe** — upstream half fixed by the previous commit; recorded side is a closed enum, never caller text |
| `name` | was `TrimSpace`d | **DEFECT — fixed here** |
| `start_date` / `end_date` | `Format(campaignDateLayout)` | **safe** — both sides reduced to `YYYY-MM-DD`, and symmetric: `googleAdsDateOnly` parses strictly and passes an unparseable value through whole |
| `channel_type` | `googleAdsRecordedChannelType` | **DEFECT — fixed here**, second finding below |
| `status`, `budget_delivery`, `budget_shared`, `bidding_strategy` | `nil` | **safe** — never compared |

`googleAdsRecordedChannelType` does trim and lowercase, but onto a FIXED constant (`"SEARCH"`,
`"DEMAND_GEN"`) rather than onto caller-derived text, so the compared value is canonical by
construction. `{"channel":"  SEARCH  "}` is pinned as a match case for that reason. **Symmetry
is the invariant, not "never normalise":** what must not happen is a normalisation applied to
one side of a string comparison only.

Google Ads is the only adapter implementing `CompareSettingsField`, so the sweep is complete at
one file.

## A default is only sound over the population it was reasoned about

`googleAdsRecordedChannelType` decoded the snapshot into `googleAdsConfig`, whose
`Channel string` cannot distinguish three different things:

| snapshot | decodes to | was reported as |
| --- | --- | --- |
| `{"budget":50,"channel":""}` — a caller predating the field | `""` | `SEARCH` ✅ correct |
| `{"budget":50}` — no `channel` key at all | `""` | `SEARCH` ❌ nobody recorded that |
| `{"budget":50,"channel":null}` | `""` | `SEARCH` ❌ |
| `{"objective":"OUTCOME_TRAFFIC"}` — not this adapter's config | `""` | `SEARCH` ❌ |

Only the first row is the legacy caller the default exists for. And `UpdateCampaign` persists
arbitrary caller-supplied `config` JSON, so the others arrive from an **untrusted request** —
the readback then manufactures a match or a divergence against a value nobody wrote, breaking
the rule the whole endpoint rests on: report agreement only when BOTH sides were actually read.

### What "recognisable" means here, and why

Recognisability is decided BEFORE the default, on a property this adapter guarantees rather
than on a heuristic: **`applyCampaignConfig` marshals the `googleAdsConfig` STRUCT VALUE whole**
— on both the create (`googleads.go:391`) and the adoption (`:454`) path — **and no field on
that struct carries `omitempty`**. So a snapshot this adapter wrote ALWAYS contains the
`"channel"` key, even when its value is `""`. Presence of the key is therefore exactly
equivalent to "written by this adapter", which is the population the legacy default was
reasoned about.

Alternatives considered and rejected: requiring a `budget` key (optional in practice and shared
with other platforms' configs), or requiring N-of-M known keys (a threshold with no principled
value). Key presence is the only test that is *equivalent to* the property rather than
correlated with it.

The snapshot is now decoded into `map[string]json.RawMessage` first, because decoding straight
into the struct is what collapses the three cases. A missing key, a non-string value and an
explicit `null` each yield `nil`, which the comparator renders as `unknown`.

**Absence still means what it meant before this change.** The default was never "an empty
string means Search"; it was "a caller predating the field means Search", and such a caller's
row was written by `applyCampaignConfig` and so HAS the key holding `""`. Those rows still read
`SEARCH` — the case is pinned by two fixtures, including the full marshalled shape. What
changed is that a blob which never carried the key no longer borrows a justification that was
never about it.

### A trap that survived the first version of the fix

`json.Unmarshal` of `null` into a `string` **succeeds** in Go and leaves the zero value, so the
decode-error check did not catch `{"channel":null}` — it still took the SEARCH default. That
gap was caught only because the test table enumerated `null` separately from the wrong-typed
values; a table testing "malformed JSON" generically would have missed it. An explicit
`null` check now runs before the decode.

## A test that agreed with the bug

`TestGoogleAds_ReadSettings_RecordedChannelTypeMatches` asserted that `{"budget":50}` — an
object with no `channel` key — recorded `SEARCH`, under the case name "absent channel". It
passed against the defect because the fixture and the implementation shared the assumption that
a missing key is a legacy caller. It is rewritten to represent the legacy population correctly,
as `{"budget":50,"channel":""}` and as the full shape `applyCampaignConfig` emits, and
`{"budget":50}` moves to the `unknown` table with the seven other absence and wrong-type cases.

Reverting the decode fails 8 subtests, every one of them reporting a recorded `SEARCH` and a
`match` for a snapshot that records nothing.

## Gates

`make test` (-race), `go vet ./...`, `go build ./...`, `gofmt -s -l`, golangci-lint and
`okfvalidate` all clean. No `design/` change, so `make apigen` is not required — the compared
field list in `design/brief.go` is unchanged; `advertising_channel_type` was already compared,
and this only narrows when its recorded side is present.

# 2026-08-20 — LFXV2-3067 the date helper's passthrough could compare equal

**Fix** — the third member of a family this PR had already closed twice. Budget rounding and
the padded ` DAILY ` period were both fixed on the principle that a value the code cannot
validate must not be reportable as `match`. `googleAdsDateOnly` was the one member still
returning an unvalidated string on failure.

## The defence was true for every case except the likely one

```go
func googleAdsDateOnly(s *string) *string {
	if t, err := time.Parse(googleAdsDateTimeLayout, *s); err == nil {
		return strPtr(t.Format(campaignDateLayout))
	}
	return s   // parse FAILED -> returned verbatim
}
```

The comment defending the passthrough argued that an unparseable value "can only ever read as
a divergence, never as a match", and that showing the operator what the platform actually said
beats an unexplained absence. The first clause is the load-bearing one, and it is false.

It holds for the malformed shapes the tests enumerated — `"2026-08-01 garbage"`,
`"2026-08-01 25:99:99"`, `"2026-08-01 00:00:00 EXTRA"`. None of those can equal the recorded
side, so returning them raw is harmless. But the recorded side is rendered by this very
function's own target layout, `campaignDateLayout` (`YYYY-MM-DD`). So the ONE malformed value
whose raw form collides with it is a bare `"2026-08-01"` — Google returning the date without
the `HH:mm:ss` its documentation promises. That fails the strict parse, is returned verbatim,
and is then byte-equal to the recorded date. The comparison reported **`match`** for a value
the code had just refused to parse.

That shape is not an exotic input; it is the most plausible way a real API drifts from a
documented datetime format. The enumerated cases in the test table were the ones that could
not collide, which is why a table with six malformed entries proved nothing about the seventh.

## Fixed by withholding, exactly as the sibling does

```go
t, err := time.Parse(googleAdsDateTimeLayout, *s)
if err != nil {
	return nil
}
return strPtr(t.Format(campaignDateLayout))
```

`nil` flows into `model.CompareSettingsField`, whose stated rule is that a verdict of `match`
or `diverged` requires BOTH sides, so an absent upstream yields `unknown`. This is the same
fail-closed path `googleAdsBudgetTypeFromPeriod` takes for a period it cannot name, and the
symmetry is the point: three fields, one rule, no member exempt.

The cost is real and worth naming — the operator no longer sees the raw malformed string
beside the recorded date. `unknown` says "this was not comparable", which is true; `match`
said "these agree", which was not. A report cannot buy display fidelity with a false verdict.

**The general rule this yields:** a passthrough-on-failure is safe only where the raw value
cannot share a spelling with the side it will be compared against. Check the comparison's
OTHER side before concluding a malformed value "can only read as a divergence" — here both
sides were rendered in the same format by design, which is precisely what made the collision
inevitable rather than unlucky.

## The existing test asserted the broken behaviour

`TestGoogleAdsDateOnly` pinned `{"a value with no time is unchanged", &dateOnly, &dateOnly}` —
naming the exact defect as correct behaviour, in the neutral language of a passthrough rather
than as a claim about verdicts. It was written to guard against unconditional space-splitting,
a real hazard, and the case that mattered was sitting in the table looking like a control.
Rewritten to expect `nil`, and the four fabrication cases now expect `nil` too.

A second test was added one layer UP, at `ReadSettings`, because the helper's return value is
not where the defect was visible — the `match` verdict is:

```
start_date comparison = "match" ... Upstream = "2026-08-01"   (before)
start_date comparison = "unknown", Upstream = nil             (after)
```

## Mutation-verified

Both reverts COMPILE, so neither is answered by a build break:

```
restore `return s` on the failure arm                 -> both tests FAIL
add a campaignDateLayout fallback parse (so the bare
  date parses and formats back to itself)             -> DateOnly test FAILS ("2026-08-01", want nil)
```

The second mutation is the one worth keeping: it reaches the same fabricated `match` by
ACCEPTING the value rather than by passing it through, so a fix that only guarded the
passthrough arm would have survived it. The defect is the comparable string, not the branch
that produced it.

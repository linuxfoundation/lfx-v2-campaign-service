# 2026-09-29 — A2: LinkedIn excluded the worst CTR from its low-CTR rule

**Fix** — behaviour change, operator-facing, LinkedIn only, plus a naming-only change on Google.
No ticket; found while auditing the per-platform thresholds, not reported.

`linkedinActionItems`' low-CTR rule was `m.Ctr > 0 && m.Ctr < linkedinLowCtrPct`, ported from
`linkedin-ads.service.ts`. **A 0% CTR is the worst possible case of the condition that rule
exists to detect, and it was the one case excluded.**

On the other three platforms nothing is excluded — Meta, Reddit and Google all read
`ctr < threshold && impressions > floor`, so a 0% CTR at volume fires. And LinkedIn is the only
one of the four with no separate "impressions but no clicks" rule (`googleActionItems` has one at
`m.Impressions > 0 && m.Clicks == 0`), so nothing else caught it either.

The result an operator saw: a LinkedIn campaign with half a million impressions and not one click
produced **no action item at all**, while a campaign at 0.29% got a MED. Verified rather than
reasoned — run against the parent commit,
`TestLinkedinLowCtr_ZeroCtrIsTheWorstCaseNotAnExemption` fails with
`no action item contained "Low CTR" among []`.

## Both halves had to land together

Removing `ctr > 0` alone would have made things worse, not better: every campaign that has not
been served yet has `Ctr == 0`, so the rule would have fired `Low CTR: 0.00%` on every new
campaign on the account. `ctr > 0` was doing two jobs — suppressing the unserved case (correctly)
and suppressing the zero-click case (incorrectly) — and only an impressions floor separates them.

This is the same shape as the Reddit underspend fix of 2026-09-28, where the clause being removed
turned out to be the only thing suppressing a false HIGH. Worth naming as a pattern: in a ported
rule engine, a guard that looks wrong is often load-bearing for a second case nobody wrote down.

`linkedinMinImpressions = 1000`, matching Google and Reddit.
`TestLinkedinLowCtr_NeedsVolumeBeforeItMeansAnything` pins the floor at 0, 1 and exactly 1000
impressions, so the exclusive boundary cannot drift silently.

## Which per-platform thresholds survive, and why

The A2 rule was to keep a per-platform number only where a reason can be stated. After this
change the low-CTR pair is 0.3 / 1000 on Google, LinkedIn and Reddit, and 0.5 / 500 on Meta —
whose CTR baseline is genuinely higher and whose delivery reaches judgeable volume sooner. That
is now written on the const blocks rather than being three coincidences and one difference.

The clicks-without-conversions floors stay per-platform (Meta 20, LinkedIn 50, Reddit 100)
because the click volumes they sit on genuinely differ; that is stated too.

Google's own `0.3` and `1000` were bare inline literals — the only two of these thresholds in the
package that were not named constants — and are now `googleLowCtrPct` / `googleMinImpressions`.
Naming only, identical values, no behaviour change.

# 2026-09-28 — A2: Google dropped campaigns for merely starting with "zz"

**Fix** — behaviour change, operator-facing, Google only. No ticket; this defect and the
no-budget one were found while reading the port rather than reported, and neither has an issue.

`EvaluateGoogleMonitor` skipped every campaign whose lowercased name began with `zz`. The intent
is real and worth keeping: the operator names scratch and archived campaigns `zz_old_draft`,
`zz 2026 planning`, and the monitor should not raise action items about them. But the check was
`strings.HasPrefix(name, "zz")` and nothing else, so it also dropped any campaign that happens to
start with those two letters — `Zzyzx Road Retargeting`, `ZZTop Sponsorship`, `zz2026`.

Two bare letters is not a convention, it is a coincidence waiting to happen. The prefix now has to
be followed by a separator — anything that is not a letter or a digit — or be the entire name:

```go
func isScratchCampaignName(name string) bool
```

`zz`, `zz_x`, `zz x`, `zz-x` are scratch; `zzy`, `zz2` are campaigns. Rune-aware, so a non-ASCII
first character after the prefix is classified rather than byte-compared.

**What makes this worth fixing rather than noting.** A dropped campaign is invisible here. It
produces no row, no action items, and nothing anywhere in the response says a campaign was
filtered — so a live campaign with an unlucky name silently has no monitoring at all, and the
absence looks exactly like the campaign not existing. There is no symptom to notice, which is why
it took reading the filter to find it.

The narrowing cannot break the convention it serves: every scratch name the operator actually uses
carries a separator, which is what the pre-existing test's three dropped names
(`zz-old-campaign` and siblings) already demonstrated — it passed unchanged against the new
helper before being rewritten.

`TestEvaluateGoogleMonitor_FiltersScratchCampaigns` keeps the filter pinned, including bare `zz`
and a space-separated name. `TestEvaluateGoogleMonitor_KeepsCampaignsThatMerelyStartWithZZ` is the
regression test; run against the parent commit in a throwaway worktree it fails with
`got 0 rows, want 3`.

Google is the only platform with a name filter of any kind. LinkedIn, Meta and Reddit monitor
every campaign the platform returns, so there is nothing of this shape to narrow there.

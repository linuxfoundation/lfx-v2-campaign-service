# 2026-10-02 — LFXV2-2023 account-scope: Meta's two id forms, and comments overstating coverage

**Fix** — First PR review round on #242, following
`2026-10-02-LFXV2-2023-account-scope-claims-vs-assertions.md`. Five findings from two bots,
each reproduced before fixing.

**Meta has two equivalent account-id forms and the guard compared raw strings** (Cursor
Bugbot, Medium). `matchesAccount`/`trimAccountPrefix` already treat `act_123` and `123` as one
account — the helper's own doc says a row and a result blob must "compare equal regardless of
which form each stored". The guard used `strings.TrimSpace` equality instead, so a legacy row
holding `777` with a request for `act_777` — the ONLY form `ValidateAccountID` accepts — got a
400 naming the request, for an account the connection genuinely manages. Reproduced across all
three (stored, requested) pairs: only `("777", "act_777")` was wrongly refused, and
`("act_777", "777")` never reaches the guard because the validator refuses a bare-digit request
first.

**The fix the finding implied was incomplete, and the new test arm is what caught that.**
Switching to `matchesAccount` made the pair match, but the guard then returned the STORED form
and `meta.Client` refused `"777"` with `must be act_<digits>` — one 400 traded for another. The
guard now returns the REQUEST's form, which is the only one guaranteed canonical, since
`ValidateAccountID` has already required `act_<digits>` of it. LinkedIn deliberately keeps
plain equality: `accountIDRE` is `^[0-9]+$` with no optional prefix, so there is no equivalence
class, and the asymmetry is noted in both helpers so neither is "fixed" by symmetry later.

**Three comments overstated provider coverage** (Copilot). `internal/domain/errors.go` and
`internal/service/connection.go` said the sentinel is answerable by *every* account-scoped
monitor read; Google Ads emits it from nowhere, its guard being deferred. This was an
over-correction: told that "Reddit only" was too narrow, the previous round swung to "every",
which overstates a security property. Both now name the three actual producers and say Google
Ads is not one, so the sentinel's absence is not read as proof a request was in scope.

**A godoc described the cross-account read this guard prevents** (Copilot).
`MetaDispatcher.ListAccountCampaignMetrics` said a monitor read "names its OWN target
accountID, distinct from whatever account the project's connection currently points at" —
true before this change, false after. It conflated the client being account-agnostic (the id
travels with the call) with the request being free to name any account (it is not). Rewritten
to separate the two.

**A test this branch claimed to have deleted was still present** (Copilot). The round-3
rewrite added `ProjectRowIsTheAuthority` but a regex excision failed silently, leaving
`SharedAccountAcrossProjects` in the file while the knowledge log said it had been replaced.
The stale test is now removed and the earlier fragment corrected. Worth noting as a mechanism:
an excision that does not assert its own effect can leave the tree and the record disagreeing,
and only a reader diffing the two would notice.

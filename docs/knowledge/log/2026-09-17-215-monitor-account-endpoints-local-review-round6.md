# 2026-09-17 monitor account endpoints — local review round 6

**Fix** — A sixth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
one nit and four Important issues, all from `general`; `repo_code` and
`repo_learnings` reported no findings:

1. `internal/platform/googleads/client.go`'s `ValidateCustomerID` doc comment
   said `customerIDRE` is declared "just below" it, but `customerIDRE` is
   actually declared above, at line 604. Fixed the comment to say "just
   above."
2. `internal/dispatch/googleads.go`'s `storedCustomerIDRE` doc comment had
   gone stale (and self-falsifying) once `googleads.ValidateCustomerID` was
   exported for the account-monitor caller-supplied path: it no longer
   explained why this regex stays separate from the client's now-public one.
   Rewrote the comment to state explicitly that the two regexes enforce the
   identical digits-only shape for two different error-classification
   purposes — a malformed *stored* value must classify as
   `domain.ErrConnectionNotUsable`, a malformed *caller-supplied* value as
   `domain.ErrAccountIDMalformed` — and that the two must be widened
   together.
3. `internal/platform/reddit/monitor.go`'s `ListAccountCampaigns` and
   `FetchAccountTotals` each inlined their own copy of the account-id shape
   check (`id == "" || !accountIDRe.MatchString(id)`) instead of calling
   round 5's new `ValidateAccountID`, exactly the divergence hazard the
   dispatcher's defense-in-depth comment warns about. Both now call
   `ValidateAccountID` instead of duplicating its body.
4. `internal/dispatch/reddit_test.go`'s
   `TestReddit_ListAccountCampaignMetrics_RejectsMalformedAccountID` used an
   empty account id to exercise "a shape-invalid account id with no Goa
   `Pattern` to catch it first," but an empty string is already rejected at
   the design layer by `MinLength(1)` regardless of `Pattern` — so the test
   didn't actually exercise the gap it claimed to. Changed the account id to
   a nonempty, charset-invalid value (`"t2/../abc"`) and updated the doc
   comment to explain why nonempty-but-malformed is the case that matters
   here.
5. `internal/platform/meta/monitor.go:205` and
   `internal/platform/linkedin/monitor.go:62` — `days` reaches each client's
   window arithmetic with no dispatcher- or client-level bound, only the
   design layer's Minimum/Maximum and the service layer's
   `validateMonitorDays` (`internal/service/connection_monitor.go:142`,
   called before either client is ever reached). Not a live bug — today's
   only caller is that service layer — but undocumented as a deliberate
   choice. Documented the asymmetry explicitly in both clients' doc comments:
   `days` is trusted pre-validated by the service layer, and a future
   non-HTTP caller of either package directly would need its own bound.

No new correctness bugs found this round; all five findings are
comment-currency, duplication, and test-fidelity fixes, plus one deliberate
documented trust-boundary decision.

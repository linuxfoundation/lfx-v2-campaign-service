# 2026-08-07 — LFXV2-2023: account-mismatch logs bypassed safeErrSummary

**Update** — Both account-mismatch branches in `internal/service/brief.go` — the metrics read and
the status toggle — now render their error through `safeErrSummary` before logging, matching every
other failure branch in the file.

**Fix** — The two branches logged the raw error while the `default:` arm three lines below scrubbed
the same class of error. That asymmetry mattered more than it looks. The error embeds
`client.CustomerID()`, which comes from the connection's `account_id`, and that design attribute
carries no `Pattern`, `MaxLength`, or charset constraint — unlike Meta's `act_<digits>` or X's
alphanumeric ids. Worse, this guard fires BEFORE any request is sent, so the client's own
`validateAccountIDs` has not run for that instance: nothing anywhere upstream of the log line had
ever inspected the string. An operator-supplied `account_id` could therefore carry control
characters and unbounded length straight into a log record.

The comments on both branches previously said only that the customer ids stay server-side, which is
about the client RESPONSE. That was true and remained true while the log leaked, which is why the
existing comment did not prevent this — the two concerns share a value but not a mechanism. Both
comments now name the log path and its reason explicitly.

One test per branch, each revert-verified: reverting either call site alone leaves the other green,
which is exactly the failure mode that let the gap open in the first place.

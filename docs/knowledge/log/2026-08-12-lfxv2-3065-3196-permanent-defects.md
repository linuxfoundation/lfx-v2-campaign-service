# 2026-08-12 — telling a caller to retry something that can never succeed

**Update** — `internal/service/brief.go`, `internal/dispatch/linkedin.go` (LFXV2-3065, LFXV2-3196).
Three permanent connection defects stop answering 503 on the campaign status toggle, and
LinkedIn's toggle and metrics paths gain the classification every other adapter already had.

## One defect, two tickets, the same shape

Both tickets are instances of the same error: a **permanent** failure reported as a **transient**
one. 503 means "retry later". None of these resolve by waiting:

- no connection row exists for this project and provider
- the stored credential blob failed GCM authentication
- the connection is inactive, its blob is not valid JSON, a required field is empty, or no ad
  account has been chosen

The cost is paid twice. A user gets a retry affordance for a state only a human edit clears, and
an operator watching 5xx rates sees platform-outage signal for user-repairable configuration.

## Three answers, not one

The toggle now separates what its `default` arm had merged:

| Cause | Was | Now | Why |
|---|---|---|---|
| No connection row | 503 | **404** | Nothing exists to repair. 409 is the "repair it" answer and would send someone to edit a row that is not there. |
| Undecryptable blob | 503 | **500** | Not the caller's scope. A rotated `CREDENTIAL_ENCRYPTION_KEY` or a corrupted row — reconnecting fixes neither, and a project admin cannot see the key. |
| Repository error | 503 | **503** | Genuinely transient. Keeps the answer the other two no longer share. |

The 500 mirrors the reasoning already written on the `ErrSystemConnectionNotUsable` arm directly
above it: a 409 tells the caller to repair *their* connection, which is wrong when the fault is in
a scope they do not own.

**I got this wrong first and it is worth recording why.** I initially answered the decrypt failure
409, having concluded that `briefs` declared no `InternalError` type and that a 500 would need a
Goa design change across three services. That was wrong: I had grepped the error types the file
*used*, not the ones *available*. `briefs.InternalServerError` was already there, three hundred
lines further down, on an arm making exactly this distinction. Grepping usage and concluding
absence is the specific mistake.

## LinkedIn was the last adapter with no helper

Six adapters route their pre-flight through one resolve/validate function and tag each defect with
`domain.ErrConnectionNotUsable` plus a reason sentinel. LinkedIn validated inline at three separate
call sites with bare errors. Two of them — `ToggleStatus` and `ReadMetrics` — take the synchronous
service mapping, so those fell to the 503 default and are what this change fixes. `Dispatch` is the
third, and it is deliberately left inline: it wraps its failures in `notCreated()` to release the
dispatch claim, a contract the shared helper does not carry (see `internal-dispatch.md`).

`resolveLinkedInCredentials` mirrors `resolveMetaCredentials` structurally — including the
`conn := res` binding the `defer` closes over, for the reason meta.go records: every not-usable
return sets the named return to nil, `systemScoped` is a no-op on a nil receiver, and reading the
named return would silently drop system-row attribution from exactly the errors that need it.

**Scoped to toggle and metrics. `Dispatch` keeps its own inline checks**, and that is not an
oversight: they wrap in `notCreated()` to release the dispatch claim, a different contract from
returning a classified error to a synchronous handler. Folding them would mean the helper had to
know which caller it had.

**One asymmetry against Meta**, worth stating because the obvious reading is that they should
match. LinkedIn emits `account_not_selected` on toggle and metrics; Meta deliberately does not.
LinkedIn's client is constructed with a `RuntimeConfig` naming the account, so an empty account id
cannot reach the platform at all. Meta targets the campaign node by platform id and never reads
the account, so an account cleared after creation must not block pausing.

## Verification

All nine new tests are revert-verified — six in `brief_test.go`, two in `linkedin_test.go`, and
one in `connection_defect_tagging_test.go`. (The count was written at four-in-`brief_test.go` and
went stale when the later attribution and redaction rounds added two more; re-derived from the
diff with `git diff origin/main -- '*_test.go' | grep -c '^+func Test'`, which is the check this
entry should have used the first time.) The LinkedIn table is 2 entry points × 4 defects, and
stripping the sentinels from the helper fails 4 subtests — so it binds the tagging, not merely the
presence of an error.

`TestLinkedIn_UnusableConnectionIsTaggedOnEveryPath` registers LinkedIn in the shared
`runConnDefectSuite`, which drives both credential scopes. The LF-system half is the one worth
having: replacing the `conn := res` binding with a read of the named return fails **only** the
`lf_system_fallback` subtests and leaves every project-owned case green — the precise signature of
the attribution bug the binding exists to prevent, and one no other LinkedIn test could see,
because they all use project-owned rows.

The docs that stated the old behaviour as fact are corrected in the same change: two rows in
`docs/api-catalog.md` said "LinkedIn emits none of them", and `internal-dispatch.md` said "still
bare, still 503" in three places. Those sentences enumerate which adapters do what, which is the
class of claim a change like this silently falsifies.

# 2026-08-18 — LFXV2-3281 LinkedIn expired-token classification

**Fix** — an expired LinkedIn access token now answers **409 "reconnect LinkedIn"** with a
`credentials_rejected` reason token, instead of the generic **500** observed live on 2026-08-14.

**The refresh flow in the ticket's scope was not built, because the stored data cannot support
one.** This is the finding, not a deferral of the work. The persisted LinkedIn credential blob
holds exactly one field, `AccessToken`: `design/connection.go`'s `linkedin-ads-credentials`
requires only `access_token`, `internal/bootstrap/sysacct.go` lists only `access_token` as
required, and `internal/dispatch.linkedinCreds` decodes only that. An OAuth refresh needs a
refresh token, a client id and a client secret; **none of the three is stored**. There is no code
path to fix — the exchange has no inputs. Persisting a refresh token is a connection-schema
change (a new credential field, a design/API change, and a re-consent of every existing LinkedIn
connection, since a refresh token can only be obtained at authorisation time), and LinkedIn
issues one only to approved applications. That belongs in its own ticket with its own review.

So the deliverable here is the ticket's item 3, which is independently correct and was the part
actually causing operator pain: the failure is now legible. Items 1, 2 and 4 remain open.

**Why this is a connection defect and not an upstream failure.** `domain.ErrCredentialsRejected`
joins the `unusableConnectionReason` vocabulary, wrapped alongside `domain.ErrConnectionNotUsable`
so `internal/service/brief.go` stops falling through to its 503 default arm. A 503 promises that
waiting might help; for a token 60 days dead it never does. It is the only reason sentinel in
that set established by CONTACTING the platform — every other one is read off stored state before
a call goes out, which is why none of them could carry this case: by every local check the row is
perfect, and the defect exists only in LinkedIn's opinion of the token.

**The 403 boundary is the load-bearing part.** `linkedin.IsCredentialRejected` matches 401 on
status alone (a bearer API returning 401 did not accept the token, whatever `serviceErrorCode` it
cites — so a code LinkedIn adds later cannot silently fall through), but 403 ONLY when the body
names the credential. A 403 is overwhelmingly a scope or ad-account permission refusal on a
resource the token is otherwise valid for; classifying a bare one as expiry would tell an
operator to reconnect a working account and keep telling them so after they did. The body is read
for classification only and never surfaced — `apiError.Error` omits it and the classifier returns
a bool — so no untrusted upstream text enters an error chain or a log.

**Expired vs revoked is deliberately not distinguished.** LinkedIn returns the same 401 for a
token past its 60-day life, one whose grant a user withdrew, and one invalidated by a password
change. The remedy is identical for all three, and a sentinel claiming a distinction its evidence
cannot support would read as a diagnosis.

**One expired LF system token is the wide blast radius.** That row is the fallback for every
project with no LinkedIn connection of its own, so a single expiry disables LinkedIn for all of
them. `systemScoped` re-attributes the error to `domain.ErrSystemConnectionNotUsable` → 500 +
ERROR log, because a project cannot repair a row it does not own; the 409 would send it somewhere
it cannot succeed while the operator who could fix it hears nothing.

**Applied to all four call sites** — create, status toggle, metrics read, account discovery. On
create the tag stays inside `notCreated`: a 401 is refused at authentication, before the create is
reached, so the claim must still be RELEASED or the brief wedges on a campaign that provably does
not exist. On toggle it is ordered after `IsOutcomeUnconfirmed`, so the claim-retaining
classification wins if the two ever collided.

**Mutation-tested.** Reverting each guard fails a test: the classifier stubbed to `false` fails
all five new test functions; a bare-403 match fails four boundary cases; dropping the reason
sentinel fails every sentinel assertion; dropping `systemScoped` fails the LF-row attribution.
The vocabulary case initially had NO failing test — `unusableConnectionReason` had no direct unit
test at all — so `TestUnusableConnectionReason` was added to cover the whole mapping, and the
mutation re-run confirmed it now fails.

One test-fixture note worth keeping: the shared `goodLinkedInCreds` token is the literal `"tok"`,
a substring of the word "token" that this path's own message legitimately contains. A leak
assertion against it fails on correct code and passes on code leaking a different token, so the
no-leak tests use a distinctive `secretLinkedInToken` instead.

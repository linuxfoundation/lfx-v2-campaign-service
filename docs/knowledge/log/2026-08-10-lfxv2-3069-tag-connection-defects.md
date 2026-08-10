# 2026-08-10 — Four stored-connection defects that answered 503

**Update** — `internal/dispatch/{reddit,twitter,microsoft}.go` (LFXV2-3069, part 1). The shared
pre-flight in each of those three adapters now tags every stored-connection defect it detects
with `domain.ErrConnectionNotUsable` plus a reason sentinel, the way Google Ads already did.
Meta and LinkedIn are part 2 and still bare.

## The bug was a missing tag, and it was invisible from inside the adapter

Each adapter checks four things before contacting a platform: the row is `active`, the decrypted
blob is valid JSON, the decoded credentials have every required field, and an ad account is
selected. All four were detected correctly. All four returned a bare `fmt.Errorf`.

The service layer has exactly one arm for an error it does not recognize, and it answers **503**.
So a connection whose credentials were saved with a missing field told the caller that a platform
had not responded — about a platform no request was ever sent to — and prescribed retrying, which
is the one remedy that cannot work: only a human editing the connection clears any of these. The
correct arm, 409, already existed at `internal/service/brief.go`; nothing reached it because the
classification is only recoverable in the layer that knows the failure was pre-send.

Reddit and X/Twitter each route dispatch, `ToggleStatus` and `ReadMetrics` through one helper, so
one tag fixed three endpoints per adapter. Microsoft has no metrics path (async Reporting API),
so it fixed two.

## Both sentinels, not one

`ErrConnectionNotUsable` selects the status. It is not enough on its own: the handler logs
`reason=` from `unusableConnectionReason`, a fixed vocabulary, and an error carrying only the
status marker logs `reason=unclassified` — the most diagnosable states in the set becoming the
least diagnosed. So each defect wraps a second sentinel too (`ErrConnectionInactive`,
`ErrCredentialsUndecodable`, `ErrCredentialsIncomplete`, `ErrAccountNotSelected`).

## The unmarshal cause is dropped, not wrapped

The previous code said `fmt.Errorf("decode reddit credentials: %w", err)`. That `err` comes from
decoding the **decrypted** credential blob, and `encoding/json` quotes its input: a
`*json.SyntaxError` names the offending character, a `*json.UnmarshalTypeError` names the field
it was reading. Left in the chain it is reachable by anything that renders or `errors.As`-walks
the error — including the 503 arm's own logging — for precisely the connection whose credentials
are malformed.

Nothing actionable is lost by dropping it. The remedy for an undecodable blob is "re-save the
credential", never "fix byte 41". The tests assert the absence with `errors.As` against both
concrete JSON error types rather than a substring match on `Error()`, because a cause still in
the chain is reachable even when the top-level string looks clean.

## The account-id branch was fixed too, though the ticket names only three

Each of the three helpers also had a bare "no account id" branch carrying the identical defect —
same wrong 503, same human-only remedy. It is tagged with `ErrAccountNotSelected`, matching the
Google Ads template. Two lines in a function already being rewritten, and it does not overlap
LFXV2-3061, which concerns Meta's Goa `Required("account_id")` and `accountDiscoveryProviders`
in different files. Leaving it would have shipped a helper that classifies three of its four
exits.

## The 409's remedy had to stop naming an endpoint

Tagging these three adapters routes them into `brief.go`'s `ErrAccountNotSelected` arm for the
first time, and that arm's message told the caller to "choose one from the connection's accounts
endpoint". Only Google Ads has one (`design/connection.go`, `list-google-ads-accounts`). Reddit,
X/Twitter and Microsoft Ads would have been sent to a route that 404s — worse than no remedy,
because the caller reads it as a service bug rather than a value they have to supply. Both
messages now say to save an ad account id on the connection, which is true of every provider.
A test asserts the string "accounts endpoint" appears in neither.

## The orchestrator's own contract comment was the last place still saying "Google only"

`internal/service/orchestrator.go` carries the reasoning for why the toggle's error switch looks
the way it does, and it named Reddit, X and Microsoft among the adapters returning bare errors
that fall through to 503 — the exact claim this change falsifies. A stale comment there is worse
than one in a doc: it is what the next person reads before deciding whether a new adapter needs
tagging. It now lists the four tagged adapters and leaves only Meta and LinkedIn outstanding, with
a note that part 2 is an extraction rather than an annotation, because neither of those two has a
shared resolve/validate helper to put the tagging in.

## Placement: after the resolve, and in the helper

Two ordering constraints, both easy to get wrong:

- `defer func() { err = res.systemScoped(err) }()` must come AFTER the `d.creds.resolve()` error
  check. `res` is nil before that point.
- The tagging belongs in the shared resolve/validate helper, not at each call site. Tagging
  per-path is how Google Ads ended up classified on discovery while its own dispatch and metrics
  callers were still bare, until the helper absorbed it.

## Verification

116 subtests: five defect fixtures × two credential scopes × every exported entry point of each
adapter (Dispatch, `ToggleStatus`, and `ReadMetrics` where wired). Each asserts `errors.Is` on both
sentinels and `errors.As` against neither JSON error type, and the `httptest` server fails the test
if the adapter reaches the network at all: rejecting locally is the property, not merely returning
the right error.

Three dimensions of the suite exist because a narrower one would have been vacuous:

- **Every entry point, not just the toggle.** "The tagging lives in the shared helper" is the whole
  design claim; driving one path leaves it unenforced. Inline the helper into a single caller now
  and the others go red.
- **Both credential scopes.** A project-owned fixture never sets `fromSystem`, so the deferred
  `systemScoped` is a no-op for it: all the project-scope cases stay green with the defer deleted.
  The LF-fallback scope is what pins it, and the project scope asserts the marker's ABSENCE, since
  a project told to repair the LF row is chasing something it cannot see. Deleting one defer fails
  22 subtests.
- **A wrong-TYPED credential field as well as `{`.** Malformed-brace input produces only a
  `*json.SyntaxError`, so a table with just that fixture never exercises the
  `*json.UnmarshalTypeError` assertion — and the type error is the one that names the credential
  FIELD.

Revert-verified against `origin/main`: every case fails there, including both leak classes.

## The no-network property was not actually enforced for two of the three

`unreachablePlatform` fails the test if the adapter reaches the network, and each dispatcher was
pointed at it with `WithBaseURL`. That covers the API host only. Reddit refreshes its OAuth token
against `www.reddit.com` and Microsoft against `login.microsoftonline.com` — different hosts,
separate `WithTokenURL` options — so a guard regressing far enough to build a client would have
sent fixture credentials to the real provider rather than tripping the server. The suite would
have depended on external networking to fail, which is the opposite of what it asserts. Both now
override the token endpoint too. X/Twitter needs no equivalent: it signs each request with
OAuth 1.0a and never exchanges a token, so there is no second host, and the test now says so
rather than leaving the asymmetry to look like an oversight.

## The pre-flight section claimed a scope one adapter does not share

"Every adapter runs the same pre-flight" stopped being true once HubSpot was registered. Its
checks are bare and it has no ad-account check at all, and it was absent from the honours-it
table, so a reader arrived at "every adapter follows this contract" with a counterexample sitting
in the adapter list six lines above. The section is now scoped to the six paid-ads adapters and
HubSpot appears in the table as `n/a` with its reason: its checks are wrapped in `notCreated`,
the pre-create classification for the ASYNC dispatch path, so they are not choosing between 409
and 503 the way the rest are. That distinction matters more than the missing row did — an empty
cell reads as work queued behind part 2, and this is not that.

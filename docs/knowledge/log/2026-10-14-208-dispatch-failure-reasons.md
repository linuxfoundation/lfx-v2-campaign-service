# 2026-10-14 — #208: dispatch failures say why, without quoting the platform

**Fix** — An async create job's failed `PlatformResult.error` was always `"platform campaign
creation failed"`. The real cause went to the log and nowhere else, so a missing Reddit
conversion pixel and a dead OAuth refresh token produced the same polled result. Now
`DispatchFailureMessage` (`internal/service/dispatch_failure.go`) reports:

- service-authored pre-flight refusals **verbatim**. These are `domain.PreflightRefusal`, now
  marked across the Reddit, LinkedIn, Meta, X, Microsoft and HubSpot clients and dispatchers
  (see the `internal/dispatch` concept for the list);
- otherwise a fixed sentence per class: no connection, LF system connection missing or
  unusable, own connection unusable (with its `unusableConnectionReason` token), decrypt
  fault, service defect, token refresh failed (`domain.TokenRefreshFailure`), platform refused
  the create (HTTP status only, `domain.UpstreamStatusCarrier`), or **could not be confirmed**
  for any retained (possibly created) failure;
- the generic string as the last fallback.

No response body, URL, query or token can reach the result. Claim release/retention, the
retained row's status and the job aggregate are unchanged. Only the message changed.
`design/brief.go`'s `platform-result.error` description is regenerated, and
`docs/api-catalog.md` documents the classes. The api-catalog, design and bootstrap paragraphs
that called the reason token "log-only" were updated, and
`TestOrchestrator_PreCreateFailureLogsAClassifiedReason`, whose trip-wire required exactly
that, now asserts that the token appears and the error text does not. Google Ads clients are
not marked (Google Ads is owned separately), so for Google only the shared classes apply.

Pre-PR review follow-ups, folded into the same change:

- Caller input is no longer copied raw into a verbatim refusal. `domain.CallerValue` quotes
  each caller or connection value, cuts it to 64 runes, replaces non-graphic runes, and
  withholds it entirely when it looks like a URL, query, userinfo or multi-line paste. Every
  marked refusal and every shared validator whose text reaches one uses it.
  `TestDispatch_RefusalsNeverEchoHostileCallerInput` sends `https://x.com/a?token=SECRET-208`,
  an over-long value and a newline through X's tweet id and LinkedIn's image URN.
- The upstream-status sentence no longer claims the create was refused, because the status can
  come from a read before it (a geo resolve, a portal GET, a find-existing lookup). A 4xx reads
  "refused the request (HTTP n) before anything was created". A 5xx or 429 reads "answered
  HTTP n before anything was created (a retry may succeed)".
- A token refresh that ended with `context.Canceled`/`DeadlineExceeded` falls through to the
  generic string instead of "may need re-authorizing".
- X's `validateDate` and `validateImageInput` refusals are now marked.

PR #335 bot review follow-ups, folded into the same change:

- `CallerValue` also withholds a value containing `=` or `&`, so a bare query such as
  `access_token=…&scope=ads` is not quoted back. It also withholds an opaque token: a run of
  32+ characters from `[A-Za-z0-9+/_-]` with a digit that is not a canonical UUID. Every call
  site's real shapes survive: dates, numeric and short ids, `urn:li:` URNs, and the UUIDs used
  for asset and project ids.
- X's composed campaign and line-item name checks are marked, along with its composed
  tweet-length check, all of which run before `verifyAccount`. A 200-rune event name passes
  its own bound but overflows the 255-rune composed name, and that refusal now reaches the
  job result. Also marked: Microsoft's connection account/customer id checks, Meta's variant
  image choice, and LinkedIn's stored-image validation. Reddit's composed-name checks stay
  unmarked because they run after the account-verify GET.


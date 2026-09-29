# 2026-09-25 Six connection tests now verify against the platform

**Fix** — Six of seven "test connection" endpoints reported a broken connection as healthy. Google
Ads, Meta, Reddit, X, Microsoft and HubSpot all answered `OK: true` whenever a credential blob
existed in the row — not that it decrypts, not that it authenticates, not that it reaches the
configured account. A refresh token revoked months earlier tested clean and then failed at
campaign creation, which is precisely the failure a connection test exists to catch. LinkedIn was
the one exception, having grown its org/account cross-check earlier on this branch.

Each of the six dispatchers now implements `service.ConnectionProber` and runs a live read with
the stored credential: Google Ads `ListAccessibleCustomers`, Meta and Microsoft `ListAdAccounts`,
Reddit `GET /ad_accounts/{id}`, X the account root, HubSpot the private-app token-info endpoint.
Where the platform enumerates, the configured `account_id` must appear in the answer; Reddit and X
read the configured account directly, which is the stronger check. HubSpot checks no account:
`portal_id` routes nothing, so its probe asks only whether the token authenticates. For the five
ad platforms an unconfigured `account_id` is a failure, since such a connection cannot run a
campaign at all, and it is decided before the upstream call rather than after it.

## Every probe resolves the project's OWN credential

`ProbeConnection` resolves through `d.creds.resolveOwned`, never `d.creds.resolve`. This is the
same trust boundary `VerifyAccountOrg` already documents, and the reason is unchanged: every other
dispatch path may fall back to the shared LF SYSTEM row under `LFX_FORCE_SYSTEM_ADS_ACCOUNT`, but
a connection TEST answered from a borrowed row reports a connection the project does not have as
healthy, and nothing in the response reveals the substitution.

Where a probe needed an existing resolve chain, that chain was parameterized by its credential
entry point (`credsResolver`) rather than copied — the shape the Reddit adapter already used, now
also in the Google Ads, Meta and HubSpot adapters. Threading the resolver keeps the two paths from
drifting: a credential rejected at dispatch cannot be accepted by the test, which is the property
that makes the test worth trusting.

The guard on this is source-derived, not behavioural (`internal/dispatch/probe_owned_resolver_test.go`,
`go/ast`): it walks every non-test file in the package, finds each `ProbeConnection` plus any
`resolve*` helper it calls in the same file, and fails if the path reaches `resolve` or never
reaches `resolveOwned`, with an explicit dispatcher list so deleting a method fails too. Source,
because the failure is silent by construction — the swap compiles, passes every functional test
that uses a project WITH its own connection, and misbehaves only for the projects that do not. The
guard was proved non-vacuous by making that exact swap and watching both messages fire.

## Two predicates, and why their ORDER is the whole feature

Each platform package exports `ProbeCredentialRejected(err)` and `ProbeInconclusive(err)`, and one
shared classifier in `internal/dispatch` consults them in that order:

| Outcome | Sentinel | Result |
| --- | --- | --- |
| rejected | `ErrConnectionProbeFailed` | `OK: false`, message echoed |
| not rejected, inconclusive | `ErrConnectionProbeInconclusive` | `OK: true` with an advisory |
| neither | `ErrServiceDefect` + `ErrConnectionProbeRequestRejected` | typed **500** |

`ProbeInconclusive` defaults to `true` for an unrecognised error — it has to, because such an
error proves nothing about the credential and the alternative is reporting connections broken on
guesses. So a revoked credential satisfies BOTH predicates on most platforms, and only the order
decides which verdict the operator sees. Reverse it and a revoked refresh token classifies as
inconclusive, maps to `OK: true`, and restores exactly the bug this change removes.
`TestProbeClass_EvaluationOrderIsLoadBearing` exists for that one line.

"Neither predicate" is deliberately not folded into inconclusive: the platform refused a request
this service BUILT, which is not a verdict on the credential, and calling it inconclusive would
silently stop testing anything the day an endpoint moves — a whole platform's connection tests
passing forever with nothing behind them.

Three token paths were split to make the predicates answerable at all. `fetchToken` in
`googleads`, `reddit` and `microsoft` previously returned one untyped error for every non-2xx from
the token endpoint, so a permanently revoked refresh fell to the inconclusive default. Non-2xx now
splits by status: `errTokenEndpointUnavailable` for `5xx` (retryable) and `ErrTokenRequestRejected`
for `4xx` (permanent). Those token errors deliberately carry STATUS ONLY, because their request
bodies hold the client secret and the refresh token.

That is also why confirmed-verdict text is authored by this service and the platform error chain
is DROPPED rather than wrapped. The rejection arm is the one class echoed verbatim to the caller;
these clients render request URLs and raw response bodies, and `meta.APIError.Message` falls back
to the raw body outright. A DO-NOT-LEAK canary asserts the drop across all three arms.

Two smaller per-platform decisions worth recording. Reddit treats a `404` on the configured
account as a REJECTION rather than a service defect, because its probe asks about that exact
account, so a `404` is Reddit answering the question rather than refusing the request. And
Microsoft's `ProbeInconclusive` deliberately omits the `isPreSendDialError` arm its siblings carry:
that client renders the cause through `safeCause` into a plain string instead of wrapping it with
`%w`, so the dial classifier cannot see through the error and the arm would assert a match that
can never happen. The classification is identical either way; only the claim would be false.
`TestProbeInconclusive_PreSendDialErrorIsNotClaimed` keeps that executable.

## The service layer

All six `Test*` methods now route through one shared helper, `testConnUpstream`, modelled on the
LinkedIn switch and classifying the same seven ways — inconclusive to `OK: true`, confirmed
failure to `OK: false` with the authored message echoed, decryption failure and service defect to
a typed 500 carrying no error text, a datastore read failure to 503 (the one retryable outcome
here), an unusable connection to `OK: false` with a fixed per-provider remedy, and anything
unrecognised to `OK: false` with fixed text and the detail in the log.

The echo stays an ALLOWLIST rather than a default, because every class that reaches this switch
without an arm of its own would otherwise inherit the echo simply by not matching one. The
unusable-connection arm matters for the same reason from the other direction: one of its
conditions is detected by decoding the DECRYPTED credential blob, and `encoding/json` quotes its
input, so echoing there would put credential-derived bytes into an HTTP body for exactly the
connection whose credentials are malformed.

`ConnectionProber` is declared REQUIRED, unlike `OrgReferenceVerifier` beside it — there is no
per-platform support table, because every platform can be asked whether its credential still
works. A missing dispatcher, or a registered one that does not implement the interface, answers
`ErrServiceDefect` + `ErrConnectionProbeUnwired`, never nil: nil would report `OK: true` having
reached no platform at all, reintroducing the defect through a wiring mistake.

Two new `accountDiscovery` descriptors were added rather than reused. `redditAdsConnectionDiscovery`
and `hubspotConnectionDiscovery` name providers that already had one —
`redditAdsAccountDiscovery` (`operation: "account monitor"`) and `hubspotEmailDiscovery`
(`operation: "email search"`) — and borrowing either would have labelled the connection test's
messages and log lines with a surface the caller never touched.

Finally, `testConn`'s own baseline message claimed no upstream verification had run, which became
false on all seven paths. It was made neutral rather than removed, since deleting it would make a
nil `Message` the signal; every caller replaces it.

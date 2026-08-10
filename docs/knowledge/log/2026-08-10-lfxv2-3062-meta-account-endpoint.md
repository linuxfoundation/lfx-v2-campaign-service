# 2026-08-10 — The Meta account picker: one handler, two names

**Update** — `GET /projects/{project_id}/connection-meta-ads/accounts` (LFXV2-3062), the
endpoint half of Meta ad-account discovery. The client half landed in LFXV2-3060; this is the
dispatcher adapter, the Goa method, and the service handler that join it to the API.

## The status mapping is shared, and that is the point

`ConnectionService.ListGoogleAdsAccounts` and `ListMetaAdsAccounts` are each three lines over
one `listAccounts` helper. The de-duplication is incidental. What matters is that the switch
in the middle encodes four judgements that are individually easy to get wrong, and getting any
of them wrong produces a response that looks fine:

- **404, not 503, for a project with no stored connection.** 503 tells the caller to retry
  something that cannot succeed until a connection exists.
- **500, not 400 or 503, for a credential blob that fails authenticated decryption.** The
  cause may be a deployment-wide key rotation rather than this one row, so 400 would send
  every operator to fix a row that is fine and 503 would promise that waiting helps. This arm
  sits ABOVE the not-usable arm precisely so an error carrying both sentinels takes it.
- **500 for an unusable LF SYSTEM connection.** The caller has no connection of their own and
  the reserved scope is unaddressable, so there is nothing for them to edit — page an
  operator, tell the caller nothing specific.
- **400, not 503, for a connection that exists but cannot be used as it stands.** No amount of
  retrying fixes an inactive connection or an incomplete credential blob.

A second copy is where exactly one of those quietly diverges, six months from now, for the
third provider. Every provider that gains discovery now gets the arms that were reasoned about
here, or none of them.

## What IS per-provider is the text, and only a text assertion catches it

The two handlers differ by an `accountDiscovery{provider, displayName, notUsableRemedy}`
value. Both fields are load-bearing in a way no status-code assertion can see:

- Wire the Meta endpoint to `googleAdsAccountDiscovery` and its 400 tells a Meta operator to
  check `login_customer_id` — a field a Meta connection does not have — while returning the
  correct status for the correct reason. The operator's next action is determined entirely by
  that sentence.
- Name the wrong provider constant and the orchestrator reaches a different dispatcher. In the
  test that is a Google Ads dispatcher with no `ListAccounts`, so the call comes back
  `ErrAccountsUnsupported` — a 400, which is a perfectly ordinary answer for a discovery
  endpoint and flags nothing.

`TestListMetaAdsAccounts_MessagesNameMetaNotGoogleAds` asserts the sentences and
`TestListMetaAdsAccounts_QueriesTheMetaDispatcher` asserts the recorded provider. The Google
Ads status-mapping tests are NOT duplicated for Meta: they exercise the same function, and a
second copy would assert only that Go still calls the function it was given.

## The system-scope guard is tested through both handlers

`rejectSystemScope` is the first statement of the shared helper, above
`resolveBackendWithOrch`. `TestListAccounts_RejectsTheReservedSystemScope` is a table over both
endpoints and builds the service with NO orchestrator: if the guard ever moved below the
backend check, the case would come back 503 instead, and the reserved scope — a GET that
decrypts the LF system credential and enumerates the Linux Foundation's own ad accounts —
would be reachable the moment the service finished warming up. A third provider inherits the
coverage by being added to that table, not by remembering to write a third test.

## Discovery does not make a Meta connection completable yet

The resolver deliberately does not require an account id: the endpoint exists to answer "which
account should this connection use?", so demanding one would make it reachable only by
connections that no longer need it.

That serves **re-pointing** today — reading the choices before a PUT moves an existing
connection to a different account. It does NOT yet serve first-time bootstrap the way Google
Ads does, because `MetaAdsConnectionConfig` still declares `Required("account_id")` and the
create is rejected as a 400 before any of this code runs.

The follow-up (LFXV2-3061) is not just dropping that requirement. Every Meta path that DOES
need an account — dispatch, the status toggle, the metrics read — has to tag its empty-id
failure with `domain.ErrAccountNotSelected`, or a connection parked mid-bootstrap answers
those calls with a generic error instead of one that names the missing choice.

Which is why `accountDiscoveryProviders` in `internal/bootstrap/sysacct.go` still lists Google
Ads alone, and now says so explicitly. Membership there is not "the dispatcher implements
`AccountLister`" — Meta does, as of this change. It is "a credentials-first row is a real
lifecycle state", which needs discovery AND the tagging. Until both exist, an account-less
Meta system row is still a dead row; it merely has a way to find out what it is missing that
nothing tells the operator to go and use.

## Related

- `docs/knowledge/log/2026-08-09-lfxv2-3060-meta-account-discovery.md` — the client walk
- `docs/knowledge/code/internal-service.md` — account discovery
- `docs/knowledge/code/internal-dispatch.md` — the `AccountLister` capability

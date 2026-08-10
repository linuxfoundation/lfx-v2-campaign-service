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

The follow-up (LFXV2-3061) is not just dropping that requirement. The one Meta path that DOES
need an account — `Dispatch` — has to tag its empty-id failure with
`domain.ErrAccountNotSelected`, or a connection parked mid-bootstrap answers a create with a
generic error instead of one that names the missing choice. It is only that one path:
`ToggleStatus` and `ReadMetrics` target the campaign node by id and both document that they
need no account id, so there is nothing there to tag.

Which is why `accountDiscoveryProviders` in `internal/bootstrap/sysacct.go` still lists Google
Ads alone, and now says so explicitly. Membership there is not "the dispatcher implements
`AccountLister`" — Meta does, as of this change. It is "a credentials-first row is a real
lifecycle state", which needs discovery AND the tagging. Until both exist, an account-less
Meta system row is still a dead row; it merely has a way to find out what it is missing that
nothing tells the operator to go and use.

## Round 2 — review fixes

Six findings from the Cursor Bugbot + Copilot sweep, all verified against the tree before
acting. Two were real defects; four were prose that had drifted from what the code does.

**The endpoint was unreachable through the gateway.** The Goa method and the handler were
wired, but `charts/.../templates/httproute.yaml` only admitted `/accounts` under the literal
`connection-google-ads` branch and the RuleSet ruled only that path. Heimdall default-denies,
so deployed, the new endpoint answered nothing. Both files now carry `meta-ads`, and
`parity_test.go` has the row — which fails in BOTH directions if a future edit touches one
file and not the other (verified by reverting each in turn: route-only gives "no RuleSet entry
authorizes it", rule-only gives "a dead rule, or a route gap"). This class is invisible to a
diff-scoped review: the defect lives entirely in files the change did not touch.

**Discovery did not attribute LF-system-row defects.** `resolveMetaDiscoveryClient` returned
its three stored-state failures as plain `ErrConnectionNotUsable`, so a project running on the
system fallback got a 400 telling it to edit a connection it does not own, instead of the 500
that pages the operator who installed the credential. Fixed the way the Google Ads path was:
a named return plus `defer func() { err = res.systemScoped(err) }()`, so a fourth return added
later cannot forget. `TestMeta_ListAccounts_AttributesSystemRowDefects` covers all three
defects × system/project.

**The remedy text named the wrong field.** The 400 told a Meta operator to check
`accessToken` — the Go field name of the persisted blob. The operator sends `access_token`
(`design/connection.go`, `MetaAdsCredentials`); the storage name is unaddressable from
outside. `notUsableRemedy` now documents that it carries PUBLISHED field names. Dispatch-layer
messages still name the Go field, correctly: their audience is this codebase.

**Two claims in the prose were false.** "Every Meta path that needs an account — dispatch, the
status toggle, the metrics read" was wrong: `ToggleStatus` and `ReadMetrics` both target the
campaign node by id and say so in their own comments, so `Dispatch` is the only one, and
LFXV2-3061 tags one call site rather than three. And "Google Ads is the only provider with a
discovery endpoint" stopped being true in this very change; what is still true, and is the
actual reason `GoogleAdsConnectionConfig` is alone in relaxing `Required("account_id")`, is
that discovery is necessary but not sufficient — the account-needing path must also fail with
`account_not_selected`. Corrected in `design/connection.go`, `internal/bootstrap/sysacct.go`,
`internal/dispatch/meta.go`, `docs/api-catalog.md` and
`docs/knowledge/code/internal-dispatch.md`, and in the section above.

Also: `metaAccountsServer` wrote the captured request path from the httptest handler goroutine
with no mutex. Now a `recordedPath` with one, matching the rest of the file.

## The shared account type published one provider's id format to both

`AccessibleAccount` is the result element for every provider's discovery method, and its `id`
attribute carried `Example("8666746580")` — Google's bare-digit customer id. Goa copies an
attribute-level example into EVERY schema built from the shared type, so adding the Meta method
here republished Google's format as Meta's, on the same endpoint whose own description promises
`act_`-prefixed ids. A generated client scaffolded from the example would have stored a value
Meta's own connection validation rejects.

The example is gone rather than corrected: no single value can be right for a type shared across
providers, and splitting into a Meta-specific type would force `listAccounts` — the shared
status-mapping helper that is the whole design point of this change — to be duplicated or made
generic. Both formats are now stated in the attribute DESCRIPTION, which can name each provider.
This drops one `example:` line from 8 response schemas and shifts Goa's example-RNG stream, so
`gen/` churns more widely than the design change implies; `make apigen` run twice is
byte-identical, so the churn is deterministic, not a rebuild artifact.

## Three concept files still said Google was the only provider with `/accounts`

The chart changes here (`httproute.yaml`, `ruleset.yaml`, `parity_test.go`) admit a second
discovery path, which under CLAUDE.md step 1 obliges the concept files that describe them:
`kubernetes/httproute.md` documented the alternation branch as the literal
`connection-google-ads`, `kubernetes/ruleset.md` called `/accounts` "the ONLY provider-specific
`connection-*` sub-path", and `code/internal-bootstrap.md` explained `accountDiscoveryProviders`
as "Google Ads alone" with discovery as the criterion. The last is the one that could cause harm:
membership is NOT "the dispatcher implements `AccountLister`", so the next person adding a
provider would have read the concept file, seen the criterion satisfied, and added a row to a map
whose actual precondition (the account-needing path fails with `account_not_selected`) they had
never been told about.

## Three Google-specific comments in a now-shared handler

`internal/service/connection.go` acquired the Meta path but kept prose written when Google was
the only caller: "The SEVENTH endpoint" (eight, and the count was never the point — the property
was reaching the repo without a `connection_handler.go` helper), "Google is never contacted" in
the arm classifying pre-send failures, and a `len(customers)` justification for preallocating the
result slice, which is Google's variable name. All three now describe the shared path.

## Related

- `docs/knowledge/log/2026-08-09-lfxv2-3060-meta-account-discovery.md` — the client walk
- `docs/knowledge/code/internal-service.md` — account discovery
- `docs/knowledge/code/internal-dispatch.md` — the `AccountLister` capability

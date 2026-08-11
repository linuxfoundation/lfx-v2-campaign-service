# 2026-08-09 — The bearer token is verified, not decoded (LFXV2-3053)

**Update** — the bearer token is now verified against Heimdall's JWKS instead of being
base64-decoded and trusted.

**The gap.** `JWTAuth` split the token on `.`, base64-decoded the middle segment and
believed it — no signature, issuer, audience or expiry check. The config for doing better
was wired end to end (the chart injects `JWKS_URL` and `JWT_AUDIENCE`, `LoadConfig` reads
them into `JWKSUrl`/`Audience`/`Issuer`) and **nothing consumed them.** Heimdall validates
every route, so closing this only earns its keep when something reaches the pod without
passing the gateway — what settles it is not the probability of that but the consequence:
the principal is written to `created_by`/`updated_by`, so an unverified claim was a
forgeable audit trail for who authorized paid ad spend.

**What was built.** `internal/infrastructure/auth`, following
`lfx-v2-query-service`/`lfx-v2-meeting-service` rather than hand-rolling JWKS handling:
PS256 pinned, issuer `heimdall`, audience `lfx-v2-campaign-service`, 5-minute key cache,
5-second skew, non-empty `principal` required, one `ErrUnauthenticated` sentinel. In
`internal/service` one embedded `authGuard` serves all three services, and
`actorFromToken` — the decoder — is deleted.

**Three decisions that could have gone the other way.** *One PR, not "package first, wire
later"*: an unreferenced verification package passes every gate and secures nothing. *A
nil verifier rejects rather than falls back*: decoding-as-before would put the old
behaviour one wiring mistake away, so failing closed makes it an outage — and
`TestNewContainer_AllPathsInjectTheTokenVerifier` makes it a test failure first, since the
degraded paths construct theirs independently — and, for the live path a unit test cannot
boot, `TestNoServiceIsConstructedOutsideItsVerifierInjectingHelper` asserts in the SOURCE
that no construction escapes its verifier-injecting helper. *An
empty JWKS URL defaults, a wrong one fails the pod*: erroring on empty turned every
hand-built `config.Config` into a service refusing all traffic, while defaulting a *typo*
would hide a misconfiguration.

**Stale comments were the real hazard.** Two survived asserting the old model: the parity
test's `JWT_ISSUER` exemption ("empty means no issuer verification" — the check is now
unconditional and empty selects the default) and `attributedActor`'s doc.

**What this supersedes.** The 2026-08-07 entry (LFXV2-3038) accepted that *a missing actor
does not fail the write*, because rejecting would escalate a token-**decoding** regression
into a total outage of brief creation. That premise is gone: the token is now verified, so
what a NULL-attributed row records is an unauthenticated request. `JWTAuth` refuses before
any handler runs, making the nil branch unreachable through the served routes; the warning
it emits is kept as a tripwire for a future entry point wired without the security scheme.
That entry's own file is left untouched — one file per entry.

**Two library defaults that do not hold on their own.** `validator` checks `exp` only when
the claim is PRESENT, so a signed token omitting it would never expire — `VerifyActor`
rejects a zero expiry itself. And `jwks.NewCachingProvider` defaults to an `http.Client`
with no timeout while the cold fetch holds the provider's write lock, so the key fetch is
given a bounded client.

**The bypass that survived the rewrite.** Verifying the token is worth nothing if the
process can be told not to. `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` disables verification
outright, and "local development only" was a comment, not a control: the chart declares the
key under `app.environment` and `deployment.yaml` renders whatever it holds, so a `--set`
or an edited values file could ship a running pod accepting ANY bearer token as that
principal — on the endpoints that spend money. The empty default and the parity exemption
keep the chart honest about *its own* value; neither can stop an override.

`New` now refuses the mock principal when the process detects a cluster. A laptop,
`go run`, a plain container and CI do not, so the workflow this switch exists for is
untouched.

**The first version of that detection was `KUBERNETES_SERVICE_HOST != ""`, and the reason
given for it was wrong.** The claim was that the kubelet injects the variable and the
chart cannot unset it, so the same override that enables the bypass cannot also conceal
the cluster. But `deployment.yaml` renders every key of `app.environment` and appends
`app.extraEnv` verbatim, and an explicit container `env` entry takes precedence over the
kubelet's service variables — so a single override could set the principal *and* declare
`KUBERNETES_SERVICE_HOST: ""`, producing a pod that accepts any bearer token as that
principal. The guard was defeated by exactly the input it was guarding.

Worth naming as a class: the property being relied on was "the chart cannot express this",
and that was never verified against the template. A rendering loop over an arbitrary map
can express *any* variable name, which makes "the environment says so" a weak foundation
for any security discriminator.

The replacement does not rest on one layer:

- **Template time** — a guard at the top of `deployment.yaml` `fail`s the render when
  either env input declares a `KUBERNETES_*` name or gives the bypass key a non-empty
  value. The deploy cannot be produced, so no cluster ever runs it. The first version of
  this guard read `.value` only, which left the same hole open through `valueFrom`: both
  env inputs support secret and field references, and a Secret's contents are invisible to
  the template, so an empty-looking check would have rendered a Deployment whose principal
  arrives at runtime. The form is now refused outright for this key — inspecting what the
  template cannot see is not an option, and rejecting `valueFrom` for one local-development
  variable costs nothing.
- **Runtime** — `config.runningInCluster` ORs the variable with the presence of the
  projected service-account directory, a signal the environment cannot express. This
  covers what the chart guard structurally cannot: manifests applied outside Helm.

Whitespace is treated as unset in both places, matching `LoadConfig`'s `TrimSpace` — a
value the service ignores must not fail a deploy.

Each guard was revert-checked. Dropping the service-account signal fails
`TestRunningInCluster/service-account mount only, variable cleared` with `= false, want
true`; dropping the template guard makes `helm template --set
app.environment.KUBERNETES_SERVICE_HOST.value=` succeed, which
`TestDeploymentRejectsReservedAndBypassEnv` reports as rendering an env block that can
disable authentication.

The refusal is an **error, not a silent downgrade** to real verification. Starting anyway —
verifying, serving, saying nothing — would leave the request path safe and the intent live
in a values file, and the next deploy of a build without this guard would ship the hole with
nothing having ever complained. Fail the pod and name the key to unset.
`TestNew_MockPrincipalIsRefusedInCluster` pins both halves: refused inside a cluster,
still honoured outside one.

**And the guard was answering a question that was not its own.** `authenticate` refused an
empty token before consulting the verifier. For a deployed pod that is a no-op — the real
verifier rejects `""` explicitly — so the only verifier it ever overrode was the local mock,
whose entire contract is that every request is that principal. Goa passes `""` for an absent
`Authorization` header, and a developer running without Auth0 has no header to send, so the
mock worked only for callers who had invented a dummy token: the workflow it exists for was
the one case it did not serve. The check is gone; who may authenticate is the verifier's
question, and `TestAuthenticate_EmptyTokenIsTheVerifiersCallNotTheGuards` pins that a
verifier accepting `""` is honoured while the existing half keeps the ordinary rejection.

**One more library default that does not hold.** The bounded client stops a stalled fetch
from hanging forever; it does not stop N of them queueing. `CachingProvider.refreshKey`
takes the write lock and fetches without re-checking the cache, so concurrent cold misses
serialize rather than share — the Nth caller waits N × fetch, on a path every request takes.
`coalesceKeyFunc` collapses them with `singleflight.DoChan`; `context.WithoutCancel` inside
the shared call keeps the first caller's cancellation from failing the rest, which
`TestCoalesceKeyFunc_LeaderCancellationDoesNotFailFollowers` pins.

**The fan-in test first asserted a RANGE, and the range was still flaky.** The reasoning for
it — that no barrier can observe the instant a goroutine is inside `singleflight`, following
`x/sync`'s own `TestDoDupSuppress` — was correct about the barrier it used and wrong about
the conclusion. Signalling from the caller goroutine just before it enters the wrapper leaves
every goroutine free to be descheduled in that window; once the parked fetch is released,
each can then run to completion before the next enters, and the count reaches exactly
`callers` with the wrapper working perfectly. `n < callers` narrows that window without
closing it.

The barrier that closes it runs on the other side of the call. Signal from INSIDE the fetch
and receiving the signal proves a flight is active; park the fetch and it stays active.
Followers then join deterministically, because `coalesceKeyFunc` calls `group.DoChan`
SYNCHRONOUSLY before its select and `DoChan` on an in-flight key does not invoke `fn`: a
follower whose context is ALREADY cancelled has provably joined by the time it returns
`ctx.Err()`, and its return is something the test can wait for. The assertion is now `== 1`,
and 50 `-race` runs are clean. Only the first fetch parks — parking a second would turn the
failure into a hang instead of a count. The half that genuinely cannot be made count-exact,
"every caller comes back with the keyset", moved to its own test that asserts only that.
Unwrapped, the revert-check reports 17 fetches where 1 is required.

**Known follow-up.** Rejections surface as **400**, not 401 — the design declares no
Unauthorized type and `commonBriefErrors` documents 400 as the JWTAuth rejection status;
adding 401 is a design change with generated-code blast radius, filed separately.

## Review round: the leader-cancellation test was the same flake, one test over

The fan-in test was made exact earlier in this branch. Its sibling —
`TestCoalesceKeyFunc_LeaderCancellationDoesNotFailFollowers`, which pins
`context.WithoutCancel` — had the identical defect and was missed because its assertion
LOOKS like it is about the wrapper. It is not: it reads the regression off the FOLLOWER's
error, and the follower is a goroutine racing the leader's flight. Arriving after that
flight completes, it starts its own with a live context, succeeds, and reports nothing —
so removing `WithoutCancel` could leave the test green.

The assertion now sits on the INNER context, which the test can order deterministically.
The fetch parks on `release`, closed only AFTER the leader is cancelled, and records
`ctx.Err()` when it resumes: by then a context carrying the leader's cancellation is
definitely cancelled, and one stripped of it definitely is not. The follower assertion
stays, because it is what a user of the wrapper actually observes, but the guarantee no
longer rests on it. Only the first invocation parks — parking a second would convert the
failure into a hang, which is the trap the fan-in fix already ran into. With
`WithoutCancel` removed the test now fails 20 runs out of 20.

The pattern worth carrying: when a test's subject is a property of a SHARED call, asserting
it through one participant's observable outcome inherits every scheduling race between
them. Assert inside the shared call, at a point the test controls.

## Round N: the new failure path leaked the publisher

Copilot: the `auth.New` error return lands after `newIndexPublisher` has already built a live
NATS publisher, and returns a nil container, so nothing can close it.

Correct, and the shape is worth stating precisely: this is not "the JWT branch leaks". THREE
earlier returns below it — the credential encryptor, the database configuration, and the
permanent migration failure — leak in exactly the same way and predate this PR. The JWT check
is simply the fourth, which is what makes a per-return `indexPub.Close()` the wrong fix: it
would close this one and leave the other three, and the fifth failure path added next month
would arrive leaking again.

`NewContainer` now names its results and defers a close keyed on the error. That covers all
four, and makes the correct behaviour the DEFAULT for whatever is added below rather than a
rule the next author has to know.

Not unit-tested, and deliberately said out loud rather than quietly skipped: the publisher's
connection state is not observable from outside `internal/infrastructure/indexer` — the
`*nats.Conn` is an unexported field — and the only ways to assert it are adding a production
accessor purely for the test or counting goroutines, which is flaky. The alternative Copilot
offered (construct the resource-free verifier before the publisher) is testable but fixes only
the one path.

The general form: **when a reviewer names one instance of a leak, check whether the function
has a class of it. If it does, fix the class — a point fix on a growing list of returns is a
defect scheduled for later.**

## Round N: ValidateToken answers two different questions with one error

Copilot, on `VerifyActor`: `validator.ValidateToken` does not only return verdicts on the
token — it also returns whatever the key func returned, and we were collapsing every one of
those into `ErrUnauthenticated`.

Verified before touching anything, because the fix depends on the wrapping surviving. It
does: go-jwt-middleware v2.3.1 wraps with `%w` at both layers on this path — `validator.go:192`
(`error getting the keys from the key func: %w`) and `:115` (`failed to deserialize token
claims: %w`) — so a sentinel tagged inside our own key func is readable by `errors.Is` at
`VerifyActor`. The rewritten `TestVerifyActor_UnreachableJWKSRefuses` asserts that end to end
rather than trusting the reading.

The consequence was not cosmetic. A JWKS fetch that failed, timed out, or was cancelled came
back as HTTP 400 "invalid bearer token" — to a caller holding a *perfectly good* one. Cold
cache and every 5-minute TTL expiry both reach it, and 400 additionally tells that caller not
to retry a condition that clears on its own.

So: `domain.ErrKeyUnavailable`, tagged in `coalesceKeyFunc` (both select arms, including the
caller-cancellation one — that our wait ended rather than Heimdall's answer arriving still
leaves nothing established about the token), passed through untouched by `VerifyActor`, and
turned into a third `unavailable bool` return from `authGuard.authenticate` that all three
`JWTAuth` impls map to `ConnServiceUnavailableError` (503).

Two choices worth recording.

The sentinel lives in `internal/domain`, not `internal/infrastructure/auth`. The first draft
imported the auth package into `internal/service` and contradicted the `TokenVerifier` doc
three lines above it ("an interface here so this package does not depend on the JWKS client").
`ErrConnectionNotUsable` had already set the precedent for exactly this: the service layer
classifies without importing whatever produced the failure.

And the fix was deliberately *not* applied everywhere it could have been. The nil-actor branch
— a verifier that accepts a token but names nobody — is also "our fault", but it stays on the
400 path, because `TestAuthenticate_RejectionMessagesAreOpaque` pins that a caller cannot tell
that case apart from an ordinary refusal. Moving it would have leaked the distinction through
the status code.

A side effect worth more than the fix: `TestBriefService_JWTAuth_EmptyTokenIsBadRequest` started
failing, and it turned out it had never tested its own name. Its constructor was
`NewBriefService(nil,nil,nil,nil)` — no verifier — so every run had been exercising the
no-verifier branch and the empty token never reached anything. Only splitting 400 from 503
made the two branches distinguishable enough for the test to be caught.

**A test whose constructor omits the dependency under test can pass for the wrong reason, and
nothing surfaces it until some other change makes the two paths return different things.**

## Round N: a 404 is a valid key set, and the guard against it was nearly vacuous

Copilot flagged that the JWKS fetch never checks the HTTP status. Verified in the
vendored source rather than from the finding text: go-jwt-middleware v2.3.1's
`jwks.Provider.KeyFunc` (`jwks/provider.go:84-93`) does `Client.Do` → `defer Close` →
`json.NewDecoder(...).Decode(&jwks)`, with nothing in between. `jose.JSONWebKeySet` has
one field and ignores unknown ones, so a 404 `{"error":"not found"}` decodes cleanly into
a zero-key set, is returned as a **success**, and `CachingProvider` holds it for the whole
five-minute TTL. Every valid token for those five minutes then fails to find its signing
key — which the validator reports as a token problem, so callers with perfectly good
credentials get HTTP 400 for a misconfiguration that is entirely ours.

Two guards went in: `jwksStatusGuard` (a `RoundTripper` rejecting non-2xx before the
decode, so the fetch fails and nothing is cached) and `requireKeys` (refusing a key set
with no keys at all, whatever produced it).

**The interesting part is what went wrong in writing the second one.** `requireKeys`
type-asserts the value the provider returns. The build was green, `go vet` and
`golangci-lint` were clean, and the added tests passed — but `go get` had pulled
`github.com/go-jose/go-jose/v4` into the graph for the first time, which was the tell:
the middleware decodes into `gopkg.in/go-jose/go-jose.v2` (`jwks/provider.go:13`). The
assertion was against a type the provider can never return. It matched nothing and passed
every value straight through. A guard that is a no-op, with a test suite that agrees.

The general rule, worth more than the fix: **a type assertion against a dependency's type
is only a check if it is the same major version the dependency returns.** Otherwise it is
a silent pass-through that compiles, vets and lints clean — the compiler cannot object,
because asserting an `any` to an unrelated type is perfectly legal code.

The corollary shaped the test. A test that constructs the key set itself would assert
against whichever go-jose version the *test file* imported, and would have passed against
the broken version too — vacuous in exactly the same way as the thing it was meant to
catch. `TestVerifyActor_EmptyKeySetIsUnavailable` therefore drives the **real** provider
against a real endpoint serving `{"keys":[]}`; only the provider's own value can tell the
two majors apart. Revert-verified: with the assertion pointed at a look-alike local type,
it fails with `unsupported key type/format` instead of `ErrKeyUnavailable`.

The status guard needed its own reachable assertion for the same reason, and it turned out
not to be the sentinel: `requireKeys` catches the 404 case too, so both guards produce
`ErrKeyUnavailable` and removing one changes nothing observable about the outcome. What it
changes is the **diagnostic** — without the transport guard the operator is told "JWKS
endpoint returned no signing keys", which describes a healthy issuer mid-rotation just as
well as it describes a mistyped URL. So the test asserts the HTTP status reaches the error
message, and revert-verifying confirms that is the assertion that breaks. Where two guards
overlap on the outcome, the binding assertion is on what each one uniquely contributes.

## Round N: a validity check outside the cache cannot keep the value out of the cache

Copilot filed two suppressed findings against the round above. Adjudicating them meant
reading `go-jwt-middleware` v2.3.1's `jwks/provider.go` out of the module cache rather
than reasoning from its docs, and both turned on the same twenty lines.

**The empty-key-set check was in the wrong place, and the previous round's reasoning had
been about the wrong thing.** `requireKeys` wrapped `provider.KeyFunc`, so the last round
worried at length about which go-jose major it asserted against — a real hazard, but
downstream of a larger one. `CachingProvider.refreshKey` stores the decoded value *before*
returning it (`jwks/provider.go:207-221`):

```go
jwks, err := c.Provider.KeyFunc(ctx)
if err != nil { return nil, err }
c.cache[issuer] = cachedJWKS{jwks: jwks.(*jose.JSONWebKeySet), expiresAt: time.Now().Add(c.CacheTTL)}
return jwks, nil
```

A wrapper *around* `KeyFunc` therefore rejects an empty set only after it is cached for
the full five-minute TTL. Heimdall finishing a rotation two seconds later changes nothing;
every request 503s until the entry expires. The general property: **a validity check
placed outside a caching layer cannot keep an invalid value out of the cache — it has to
run on the side the value arrives from.** The check moved into `jwksStatusGuard.RoundTrip`,
where a rejection is a transport *error* and `refreshKey` returns before it writes.

The move deleted `requireKeys`, the `go-jose` import, and with them the entire
wrong-major-assertion hazard the previous round documented. Counting `json.RawMessage`
entries off the raw body has no dependency type to get wrong. It also forced a bound
(`jwksMaxBody`): a 2xx body now has to be read in full and replayed, and an unbounded read
of an endpoint we do not control is an allocation the process cannot refuse. Exceeding it
is an error rather than a truncation — a truncated key set decodes to *fewer* keys, which
is the empty-set failure in slow motion.

The test is what pins the placement, and the previous version could not have. It asserted
`ErrKeyUnavailable` and stopped; the wrapper produced that too. The rewrite flips the
handler to a healthy key set and asserts the **next** call succeeds. Revert-verified by
restoring `requireKeys` and dropping the transport arm: it fails on the recovery
assertion, `... — the empty set must not have been cached, or recovery waits out the 5m0s
TTL`. **When a fix is about *where* a check runs, the assertion has to be about a
consequence only that location produces.** Sentinel equality was true of both.

**The second finding corrected a claim, not code.** `coalesceKeyFunc`'s godoc said the
stampede it prevents is reachable "at startup and on a TTL expiry that coincides with the
endpoint being slow". The expiry half is false: `CachingProvider` holds a semaphore, admits
exactly one refresher, and hands the **stale** set to every concurrent caller
(`jwks/provider.go:166-186`) — nobody blocks. But Copilot's version, "only an empty cold
cache", is incomplete in a way that matters. When that lone background refresh *fails*, the
goroutine runs `delete(c.cache, issuer)`, discarding the stale entry that was serving
everyone; the next N callers find a cold cache and serialize on `refreshKey`'s write lock.
So a *down* endpoint does produce the stampede that expiry alone does not — precisely when
the service can least afford it. The comment now says that. Neither party's summary was
right, and the mechanism was only visible by reading the source.

## Round N: a typed error a method does not declare is a 500

The change that made JWTAuth able to refuse a token was only half a change. `JWTAuth`
(`internal/service/connection_handler.go:42`) returns `*conn.BadRequestError` for every
token-side refusal, and that is right. But Goa generates each method's error encoder from
the errors that method **declares** in the design, and `get-`, `delete-` and `test-` in
`design/connection.go` declared only `NotFound`, `InternalServerError` and
`ServiceUnavailable`. A typed error with no `case` in its encoder falls through to Goa's
generic encoder, so a caller with a bad token got **500**, and the 400 appeared nowhere in
OpenAPI. Everything on the Go side looked correct: it compiled, the handler returned the
right error, and the only observable defect was the wire status.

Six providers × three methods, so the same omission eighteen times over — which is the
tell that the omission was structural rather than an oversight on one method. The reads and
the delete carry `bearerToken()` exactly as the writes do; the declaration follows the
security scheme, not the payload.

The test reads the **generated** encoder source and asserts a `case "BadRequest"` in every
`Encode*Error` function, rather than exercising a list of encoders by name. The hazard being
guarded is a new provider's `get`/`delete`/`test` added without the declaration, and a
hand-maintained list would not contain the new one — the single case that has to fail.
Revert-verified by dropping the declaration from `get-` alone and regenerating: seven
failures, one per provider, each naming its method.

`commonBriefErrors`/`briefErrorResponses` had already been through this and carried a
`withBadRequest bool` that no longer gated anything, `_ = withBadRequest` and all. It is
gone rather than retained-for-readability. A boolean at 38 call sites that a reader must
check does nothing is not readability; it is a standing invitation to make it mean something
again, which would restore exactly the defect above.

**Two doc claims were narrowed in the same pass.** `docs/architecture.md` said "a gateway is
a routing decision, not a security boundary a service may assume" — true of identity, false
of authorization, since the `campaign_manager` gate is Heimdall RuleSets with no in-service
equivalent. Verifying the signature buys one thing: the actor stamped on a write is the
actor the token names. And `docs/knowledge/code/internal-infrastructure-auth.md` still
preserved the TTL-expiry stampede claim that the previous round had already deleted from
`jwt.go`'s godoc — a concept doc outliving the source it describes.

## Round N+1: a test whose comment claimed a third of the coverage it had

`TestJWTAuth_UnverifiableIsUnavailableOnEveryService` opened with "pins the disposition at all
three boundaries" and went on to name the exact risk — "a split that only the brief service
honours is a split two thirds of the API does not have." It then ran `connections` and
`audiences`. The brief service, the one the comment singles out by name, had no subtest.

The generalisation is not "keep comments in sync." It is that **a test's doc comment is the
only place its SCOPE is written down, and scope is the one property no assertion checks.** A
missing assertion inside a subtest usually shows up eventually — the subtest exists, someone
reads it. A missing subtest leaves nothing behind at all; the coverage claim survives in prose
and reads, to anyone auditing by reading, exactly like coverage. That is a worse failure than
a wrong assertion, because the reader has been given a reason not to look.

The tell was available without running anything: the comment enumerated ("all three"), and an
enumeration in a comment above a table-or-subtest test is a countable claim. When a comment
counts, count.

Added the `briefs` subtest. Revert check: flipping `brief.go`'s unverifiable arm from
`ConnServiceUnavailableError` to `BadRequestError` fails it with the concrete type printed;
the other two stay green, which is the split the comment warned about, reproduced.

Also corrected `README.md` on `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL`. It read "Never set it
in a deployed environment," which is advice. The chart refuses to render it and the in-cluster
check refuses to boot, so the real behaviour is a crash-loop. Understating an enforced
constraint as a guideline costs an operator the one sentence that would explain a rollout that
never comes up.

## Round N+2: a rejected JWKS URL was logged verbatim, credentials and all

Copilot flagged `internal/infrastructure/auth/jwt.go` for disclosing a configured URL in a
startup error. Both refusal paths did it, for different reasons:

- the scheme check formatted `rawJWKS` with `%q`;
- the parse failure wrapped `url.Parse`'s error, and that error embeds the entire raw URL
  regardless of how it is wrapped.

A URL may carry credentials in its userinfo, and `cmd/campaign-service/main.go` logs the
error `New` returns, so a wrong value ships the password into the log of every pod that
fails to start. Fixed by printing `jwksURL.Redacted()` in the first case and, in the second,
reporting our own message instead of wrapping — the only way to drop what `url.Parse` put
there. Both name `constants.EnvJWKSURL`; the issuer parse had the identical shape and got
the identical treatment.

`TestNew_RejectionDoesNotLeakURLCredentials` pins both arms. Verified by reverting each fix:
the scheme arm failed with `JWKS URL "ftp://user:hunter2@h/jwks" ...` and the parse arm with
`parse JWKS URL: parse "://user:hunter2@nope": missing protocol scheme` — the second is the
one worth having a test for, because the leaking text is not in any format string in this
repo. `TestNew_ConfigHandling` now asserts the error names the config key rather than the
prose "JWKS URL".

**The generalisation: redaction has to cover the errors you inherit, not just the ones you
write.** Auditing our own format verbs would have found one of these two.

## Round N+2 (merge): main relocated the helpers this branch had already moved

`git merge origin/main` conflicted in `internal/service/connection_handler.go`. Main had
added `actorFromCtx`, `attributedActor` and `actorFromToken` there; this branch had already
moved the first two into `internal/service/auth.go` and deleted the third, because
`actorFromToken`'s best-effort unverified decode is precisely what real JWKS verification
replaces. Resolved by taking HEAD — the empty side. The only surviving reference was a
comment in `brief_actor_test.go` naming `actorFromToken` as the thing whose regression the
warning would catch; retargeted to the verifier's claims-to-actor mapping, which is what
now occupies that role.

`allowedVersionGaps[16]` was deleted: #95 merged 000016, so the excuse for the gap is gone
and `TestMigrations_AllowedVersionGapsAreStillOpen` fails while it survives. That is the
guard doing its job — the deletion did not have to be remembered.

## Round N+3: the new field the log-safe formatter did not learn about

`Config.String()` documents itself as safe for logs, and this branch added `JWKSUrl` to what it
prints — verbatim. A JWKS URL is public *by convention*, not by construction: an issuer behind a
gateway using basic auth is an ordinary deployment, and nothing here forbids configuring one.
The formatter is exactly where a convention stops holding, because it runs on whatever the
operator actually set.

The fix generalised the helper rather than adding a second one. `redactNATSURL` already solved
this shape — strip userinfo, keep the host, because the host is what makes an outage
diagnosable — so it became `redactURLUserinfo` and now serves both fields.

The class worth remembering: a redacting formatter is a **allow-list of fields someone decided
were safe at the time**, and every field added later defaults into the safe list silently. A new
config field with a URL or a credential in it has to be argued about at the formatter, not only
at the place that reads it.

## Round N+4: the standard library's definition of "redacted" is not this service's

Round N+3 above got the config formatter right and then, one file over, reached for
`url.URL.Redacted()` at eight sites in `jwt.go` — the startup scheme check and every
`jwksStatusGuard` error and debug line. `Redacted()` masks the password and **keeps the
username**, on the reasonable general view that a username identifies rather than
authenticates. That view is wrong here, and this branch had already said so: the config test
added in the previous round asserts the username never reaches a log line, because a JWKS
endpoint behind a basic-auth gateway is issued the username and password together as one
credential. So the branch shipped two formatting sites, in two packages, disagreeing about
what "redacted" means — and the one that disagreed is the one that runs on a loop. The
startup URL is rendered once; the guard renders `req.URL` on every failed refresh, so a
misconfigured endpoint writes the same line until someone notices.

The fix is a package, not an edit. `pkg/redact` holds one implementation with two entry
points — `URL` for a parsed URL, `URLUserinfo` for the path where `url.Parse` itself failed
and the raw string is exactly what must not be printed. `URL` clears `User` on a **copy**:
mutating the caller's URL would be a formatter with a side effect on the credential it is
hiding, and the next request would go out unauthenticated.

Writing the tests turned up a second, smaller thing. `URLUserinfo` splits on the last `@`,
which is right for a password containing a percent-encoded one and wrong for a NATS **server
list** — everything before the final credential is taken as userinfo, so a three-server value
diagnoses as one server. Nothing leaked; the value was truncated. But a helper whose entire
justification is that the host stays visible should not silently eat two of the three hosts,
so a comma-separated value is now redacted entry by entry. Worth recording because the bug was
invisible from the call site and only appeared once a test asserted the *whole* output rather
than "the secret is absent".

The class: **a security helper from the standard library encodes someone else's threat model.**
`Redacted()` is not wrong — it is correct for the definition of "credential" it was written
against. The question to ask of any borrowed redactor is not "does this hide the secret" but
"does its notion of secret match the one this service has already committed to elsewhere",
because the place that commitment is written down is usually a test, not a type.

## Round N+5: the fix for the lossy redactor was a leaking redactor

Four findings, and the first is mine from last round.

**The comma split leaked.** Round N+4 changed `URLUserinfo` to split a NATS server list on
commas so a three-server value stopped diagnosing as one server. `,` is an RFC 3986 sub-delim
and legal unescaped inside userinfo, so `nats://u:p,x@host` is one URL whose password contains
a comma — and the split turns it into `nats://u:p` (no `@`, nothing redacted, emitted whole)
plus `***@host`. Half a password in the log, on the exact code path whose justification is that
it never leaks. The behaviour it replaced was lossy and safe; the replacement was pretty and
unsafe.

Which a comma is cannot be decided from the value: `nats://a:4222` is either a host and port or
a user and password. So the split now happens only where the answer does not matter — every
segment has an `@`, or none does. Any mix falls back to the whole-value rule. The test case that
used to assert `nats://a:4222,nats://***@b:4222` now asserts the lossy `nats://***@b:4222`,
which is a worse-looking output and the correct one.

**Userinfo is not the only place a credential lives in a URL.** `?access_token=…` is a shape
real identity providers accept, and the JWKS URL is operator-supplied, so clearing `User` and
printing the query satisfied `net/url`'s idea of a credential and missed the actual one. Both
entry points now drop query and fragment. Ordering is load-bearing in the string form: the
strip runs AFTER userinfo removal, because a password may contain a `?` and cutting first would
truncate `nats://u:p?x@host` to `nats://u:p` — the same defect as the comma, one round later.

**Two debug lines logged the upstream response body.** Both carried a comment saying the body
is untrusted text that would travel into logs, immediately above the call that sent it there.
The argument was that a bounded length and a debug level made it safe. Neither is the relevant
property: an error page can reflect the request back — a gateway rejecting our `Authorization`
header being the likeliest case — and debug output lands in the same store as everything else.
The bodies are discarded now; the endpoint and status carry the whole diagnostic.

The class this round: **a comment that states the risk is not a control for it.** Three of the
four sites named the exact danger in prose and then did the dangerous thing on the next line.
The prose is what made them read as handled — including to me, twice, since the query strip and
the comma guard fail the same way in the same function and I shipped the second one while
fixing the first.

## Round N+6: the string that leaks is the one this package does not write

Every redaction added in rounds N+3 through N+5 protects a string this package formats. Copilot
pointed at the one it does not: `http.Client.Do` wraps *every* transport failure in a
`*url.Error` built from `req.URL`, after the guard has returned. Verified before touching
anything, because the finding turns entirely on what Go's masking actually keeps:

```
ERR: Get "https://svc:***@idp.example.com/jwks?access_token=s3cret": boom
```

The password is masked. The **username and the whole query survive**, and
`internal/service/auth.go:87-91` renders that chain with `slog` on the `ErrKeyUnavailable`
path — a reachable site, not a hypothetical one.

Two fixes were available. Sanitizing the error `KeyFunc` returns is the local one, and it is
the weaker one: it has to keep pace with the chain's shape, and it only covers the errors that
reach that call. Removing the credential from the URL `http.Client` ever sees covers refused
connections, DNS misses and timeouts alike, because it removes the material rather than
chasing the renderings of it. `New` now hands the provider a stripped copy of the URL and the
guard puts the real one back on each outgoing request.

The part that is easy to get wrong: the basic credential has to be re-applied as an explicit
header. `http.Client.send` — not the transport — is what turns URL userinfo into
`Authorization`, and it runs first, so a URL repaired inside `RoundTrip` authenticates
nothing. Which is also why there are two tests. Stripping without swapping keeps every secret
out of the error, and out of the endpoint's reach: the fetch 401s, and a 401 is an
`ErrKeyUnavailable` that satisfies the leak test completely. Revert-verified in both
directions — the leak test fails on the un-stripped URL, the auth test fails on the
strip-only one.

The class this round: **a redaction boundary drawn at the package's own format verbs.** Each
earlier round asked "does this line print the URL safely?" and the answer was eventually yes
at every site. The question that was never asked is who else formats it. The value was handed
to a library that renders it into an error on a path with no format verb of ours anywhere in
it — so the audit that walks your own `%s` sites reports clean, correctly, and misses the leak.

## The redactor leaked a token tail through the comma rule

`URLUserinfo` splits a comma-separated value entry by entry when the split is "provably safe",
and the proof was that every entry carries an `@` or none does. A comma is also legal inside a
QUERY, and the query is exactly where the other credential shape lives:
`https://idp/jwks?access_token=s3cret,b64tail` has no `@` in either piece, so the rule called it
a list — and the second piece was then `b64tail`, a bare fragment of the token with no `?` in
front of it for `trimQueryAndFragment` to cut, joined straight back into the output.

Every segment must now also begin its own `scheme://`. A comma that does not start a new URL is
a character inside the preceding value. That rejects the schemeless NATS list form
(`nats://h1:4222,h2:4222`), which is fine: it falls back to the whole-value rule, and the
whole-value rule returns a credential-free list unchanged.

## And it read an `@` in a query as userinfo

`redactOne` scanned the WHOLE string for the last `@`. Userinfo can appear only inside the
authority (RFC 3986 §3.2), so an ordinary `https://idp.example.com/jwks?contact=ops@b.example`
was rendered `https://***@b.example` — no leak, but the host and path are the only reason to
log a URL at all, and an operator reading that concludes the endpoint is misconfigured.

The scan is now bounded to the authority, and bounded at the first `/` alone. `?` and `#` are
equally illegal unescaped in userinfo, but the values reaching this function include the ones
that FAILED to parse: bounding at `?` cuts `nats://u:p?x@host` down to `nats://u:p` and logs
half a password, the same defect as splitting on a comma too eagerly. Bounding at `/` leaves
such a value inside the authority region, where the `@` rule still catches it.

One exception, because the bound proves nothing about a value holding more than one URL: an
ambiguous NATS list that the split declined arrives here whole, with later servers' credentials
outside the first authority. A second `://` past the authority start restores the conservative
whole-string rule — lossy about the earlier hosts, but it cannot print a password.

Both are revert-verified: against the previous implementation the token tail appears in the
output and the query `@` erases the host.

## Three statements that were true of a narrower thing than they described

- The no-actor warning promised "attribution will be recorded as NULL if it commits". Only an
  INSERT does that. `campaign_repo.go`'s update paths COALESCE — the upsert conflict arm, the
  replace and the soft delete — so a nil actor there PRESERVES the last actor known, and an
  operator following the warning would look for a null column the row does not have.

  The first correction over-corrected: it said "every update path", which is true of exactly one
  repository. `brief_repo.go` (replace, approve, archive) and `connection_repo.go` assign
  `updated_by` outright, so there the same nil actor CLEARS it — the third outcome. Copilot
  caught this on round 5. The warning now names none of the three and says the stored actor
  depends on the table, because `attributedActor` serves all of them. Whether the repositories
  should agree is a real question and a separate one; it is not resolved here.
- `commonBriefErrors` said JWTAuth returns `*conn.BadRequestError`. Goa generates one concrete
  type per service from the shared design type, so `BriefService.JWTAuth` returns
  `*briefs.BadRequestError` — identically shaped and a different Go type.
- The README said the mock-principal variable "cannot be set in a deployed environment". The
  runtime guard is a detection, not a proof: `runningInCluster` looks for
  `KUBERNETES_SERVICE_HOST` and the service-account directory, and `config.go` says so itself.
  A manifest that sets the variable AND suppresses both signals is not caught. The README now
  states the guard's actual shape and the bar it is built to — three deliberate visible changes
  rather than one env line.

## Both of the previous round's rules were still leaking, in three ways

Cursor and Copilot came back on `pkg/redact/url.go` with three findings, and all three are the
same shape as the two rounds above: a rule added to close one leak opened the next one out.
Each is now a case in `TestURLUserinfo_NeverEmitsACredential`, asserted both on exact output and
on the secret's own text being absent — the second assertion is the one that generalises,
because an exact-output test only fails for the shape someone thought of.

**A `://` inside a query is not a second URL.** The multi-URL fallback fires on any later
`://`, and `https://idp/jwks?redirect=https://x@y.example&access_token=s3cret` has one in a
redirect parameter — an ordinary shape in an OIDC deployment, and `Config.String` sends JWKS
URLs down this path. Read as two URLs, the output was rebuilt from the `@` in `x@y.example`,
which discards the `?` along with everything before it, so `trimQueryAndFragment` had no query
left to find and the token rode out on `&access_token=`. The fallback now requires a **comma**
as well: without one there is a single URL, and a later `://` is inside its query or path.

**The fallback was unreachable for the value most likely to need it.** It sat inside the
`at < 0` branch, so it only ran when the FIRST authority had no `@`. A mixed list is precisely
what the split refuses, so it is the likeliest thing to arrive here whole — and
`nats://u:p@a:4222,nats://u2:p2@b:4222,nats://c:4222` has an `@` in its first authority. `u:p`
was redacted, `u2:p2` printed verbatim. The check now runs before the authority `@` is handled
at all.

**A token tail containing `://` satisfies the scheme rule.** Requiring every segment to carry
its own `scheme://` was the previous round's fix for a comma inside a query, and
`https://idp/jwks?access_token=s3cret,secret://b64tail` meets it: the first segment trims at its
`?`, the second is joined straight back in. The settling test is a `?` or `#` **before** the
first comma — everything after the start of a query or fragment belongs to it (RFC 3986 §3.4,
§3.5), so no comma past that point can be a delimiter. Unlike the every-or-none and
all-schemed rules, this one is not a heuristic about what a comma might be; it is a grammar
fact about where it sits.

The recurring class, now three rounds deep: **each of these rules infers structure from a
value that is in this function precisely because its structure could not be trusted.** Every
one held for the shape it was written against and broke on the next. What finally stopped the
sequence was not a smarter inference but a cheaper one — a positional fact from the grammar,
and a conservative branch that costs earlier hosts rather than trying to keep them.

## Round 5: `isPort` answered a question its caller could not ask

Copilot found, and dealako confirmed as the round's one blocking item, that
`pathClosesAuthority` still leaked through the branch the previous round had left alone.

The rule was: an unbracketed `:` before the path is the tell, because the right-hand side of a
real authority's colon is a decimal port — so `u:p/path?x@host` is refused (`p` is not a port,
therefore `u:p` can only be userinfo) while `idp.example.com:8443/jwks?…@…` keeps its host.

A numeric PASSWORD is equally legal. `nats://u:1234/path?x@host:4222` has `u:1234` before the
`/`, and `u`-host-port-`1234` and `u`-user-password-`1234` are the same eleven bytes. The
authority reading won by default, so the `?` was called genuine, the value was cut there, and
`nats://u:1234/path` went to the log with the password in it.

There is no sharper test available at that position — `isPort` distinguishes a well-formed port
from a malformed one, and the question here is which of two well-formed readings applies. So an
unbracketed `:` is now refused whatever follows it. Bracketed hosts keep their port test:
RFC 3986 §3.2.1 excludes `[` and `]` from userinfo, so there the authority reading is the only
one.

The cost is bounded and lands the right way round. `pathClosesAuthority` is consulted only for a
value that ALSO carries an `@` past its `?`, so what is lost is the host of an explicit-port URL
with an `@` in its query (`https://idp:8443/jwks?contact=ops@b` now prints `https://***`), and
what is kept is every numeric password. The commoner `https://idp:8443/jwks?access_token=…` is
untouched — it has no `@` and never reaches the test. Both are now in `TestURLUserinfo_Shapes`,
and the leak is a revert-verified case in `TestURLUserinfo_NeverEmitsACredential`
("a numeric password is not a port"): restoring `isPort` there prints `nats://u:1234/path`.

This is the same class as the three above and it is the fourth instance: a rule that inferred
structure from a value whose structure is untrustworthy. The pattern in the resolutions is
consistent too — each one ends by giving up a distinction rather than making a finer one.

## Round 6: userinfo does not need a colon

Copilot found that round 5's fix left the mirror-image branch leaking. `pathClosesAuthority`
refused an unbracketed `:` whatever followed it, and then returned `true` for a region with no
`:` at all, on the reasoning that a bare host "holds nothing that could be a password".

Userinfo does not need a colon. `nats://token@host:4222` is a documented NATS form and this
service's own `NATS_URL` accepts it, so a colonless region is a WHOLE credential rather than a
username missing its other half. `nats://s3cret/path?x@host:4222` therefore printed
`nats://s3cret/path`.

Both malformed readings — `u:p/path?x` and `s3cret/path?x` as userinfo — put a `/` and a `?`
inside userinfo, which RFC 3986 §3.2.1 excludes. Neither is more malformed than the other, so
refusing one and accepting the other was never principled; the rule had simply not been asked
about the second shape. An unbracketed region before the path is now refused whatever it
contains, which reduces `pathClosesAuthority` to its one real proof: a bracketed host, because
`[` and `]` are gen-delims §3.2.1 excludes from userinfo, so a value opening with `[` CANNOT be
userinfo. That is a fact about the grammar; the two rules that leaked were guesses about bytes.

The cost is the same shape as round 5's and slightly wider. Only a value carrying an `@` past
its `?` consults this, so what is lost is now the host of ANY unbracketed URL in that class:
`https://idp.example.com/jwks?contact=ops@b.example` prints `https://***`. The commoner
`https://idp.example.com/jwks?access_token=…` has no `@` and never reaches the test — both are
pinned in `TestURLUserinfo_Shapes`. The leak is a revert-verified case in
`TestURLUserinfo_NeverEmitsACredential` ("a token-only userinfo is not a host"): restoring the
colon test there prints `nats://s3cret/path`.

Fifth instance of the class, and the second in a row where the FIX was the next leak. The tell
both times was the same: the rule proved an authority from the ABSENCE of a disqualifying
feature (a non-numeric port; then a colon), which proves nothing about a value whose structure is
untrustworthy. Only the bracket test ever argued from presence, and it is the one still standing.

## Round 7: the same rule, one caller away

**Kind:** Fix

Cursor found that round 6 fixed `pathClosesAuthority` and left the leak standing in its other
caller. The multi-URL branch did not ask `queryIsGenuine`; it asked `passwordCouldSpanQuery`,
which was `!pathClosesAuthority(seg) && strings.ContainsRune(seg, ':')` — the colonless reading
round 6 had just retired, re-derived locally and ANDed on top of the corrected function. So
`nats://s3cret/path?x@host:4222` redacted whole while
`nats://s3cret/path?x@host:4222,nats://c` still printed `nats://s3cret/path`.

The narrower test was deliberate and its justification was economic: refusing in a single URL
costs one host, refusing in a list costs every host in it, so the list branch "pays for a
sharper discriminator". The reasoning is sound and the discriminator it bought was unsound —
cheapness is not a reason to keep a rule already known to leak. `passwordCouldSpanQuery` is
deleted and both branches now call `queryIsGenuine`.

The priced cost lands on three cases that previously kept their hosts, none of which leaked
before and none of which leak now; each is redacted whole instead:

- `https://idp/jwks?contact=ops@b.example,https://x&access_token=s3cret`
- `nats://a,nats://b?access_token=prefix@live-secret,nats://c`
- `nats://a:4222,https://idp.example/jwks?contact=ops@b.example&…`

The third case previously existed to prove the branch was not "refuse every multi-URL query
outright". A path no longer proves that, so it is rewritten onto the one distinction that
survives — a BRACKETED last segment, `nats://a:4222,https://[2001:db8::1]/jwks?…`, which keeps
both hosts because `[` cannot begin userinfo. The leak itself is a revert-verified case, "a
token-only userinfo is not a host in a list either".

Sixth instance of the class and the third consecutive round where the previous fix was the next
finding — but with a different tell. Rounds 5 and 6 were the wrong RULE. This one was the right
rule applied in one place and independently restated in another, where the restatement went
stale the moment the original was corrected. A rule with two implementations has none.

## Round 7b: a JWKS redirect could step out of TLS

**Kind:** Fix

Copilot found that `redirectTarget` accepted any `http` or `https` Location regardless of the
current hop's scheme, so an `https` JWKS endpoint could be redirected to plaintext `http`.
`auth.New` permits an `http` JWKS URL for local dev, which is what made both schemes individually
acceptable and hid the pair.

Dropping the `Authorization` header — already done on every hop — does not cover this. That
protects the credential sent OUT, and the risk is what comes BACK: nothing signs a JWKS, so the
response body IS the trust anchor. An on-path attacker on a plaintext hop can substitute a key
set and mint tokens this verifier accepts.

`https` → `http` is now refused. The other three pairs are permitted and asserted, because a
guard that refused all four would pass a downgrade-only test while breaking the local-dev
endpoint: an operator who configured plaintext has already accepted it, and `http` → `https`
strictly improves on what they asked for. `TestRedirectTarget_RefusesATLSDowngrade` is a unit
test on the function, since the scheme pair is the entire subject and an end-to-end version would
need a TLS server this transport trusts in order to assert the same thing.

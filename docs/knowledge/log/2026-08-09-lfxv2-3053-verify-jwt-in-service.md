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

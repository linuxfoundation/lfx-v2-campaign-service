---
type: "Code Concept"
title: "internal/platform/llm"
description: "Client for the LF LiteLLM proxy's OpenAI-compatible /chat/completions surface: the service's only route to a model, with redirect refusal, per-attempt deadlines, 429 retry and credential-safe errors."
resource: "internal/platform/llm"
---

# internal/platform/llm

The service's **only** route to a model. Everything generative that lands later — email copy
first (LFXV2-2775) — goes through `Complete`, so the decisions here are made once rather than
per feature. It is a `platform/` package for the reason the others are: it speaks one vendor's
HTTP, holds no domain vocabulary, and reads no environment variables (`Config` is injected). A
package that knew what a brief was would put prompt composition here, where the domain that owns
the prompt cannot see it.

## The credential rides in a header, which is what shapes the error handling

The key is in `Authorization`, the *request body* is a prompt built from operator-supplied
campaign context, and the *response body* is model output derived from it. Neither direction is
safe to quote, so a non-2xx error names the status and nothing else, and a malformed 200 is not
quoted either — a proxy fronting a model has been observed to echo request context into its
error envelope.

**Transport errors are REBUILT, not forwarded.** `redactTransport` maps onto values this package
constructs — the canonical `syscall` and `context` sentinels, a `*net.DNSError` rebuilt from its
boolean bits — and defaults anything unrecognised to a sentinel. Peeling `*url.Error` layers
would not be enough: a custom `RoundTripper` returning `fmt.Errorf("Bearer %s: %w", key, cause)`
is wrapped by `http.Client.Do`, so the credential-bearing layer is a `*fmt.wrapError` that
survives stripping the outer one and stays reachable by anything doing `errors.As`.
`TestComplete_TransportErrorLeaksNothingAtAnyLayer` walks **every** layer via `errors.Unwrap`
rather than the outermost `Error()`, which is the only form of the assertion that catches this.
The sentinels survive the rebuild (`errors.Is` still matches) because callers distinguishing "we
gave up" from "the proxy did not answer" branch on them.

## Redirects are refused, and injection cannot drop that

Following a 3xx would resend the bearer credential to whatever host the proxy named.
`CheckRedirect` is therefore set by `NewClient` **and re-imposed by `WithHTTPClient`** on a copy
of the injected client: injection normally supplies a *transport* from an author not thinking
about redirect policy, so inheriting its policy would make a security property depend on every
future call site remembering it. The copy matters — mutating the caller's `http.Client` would
change behaviour for every other user of that value. `TestComplete_RedirectIsNotFollowed`
asserts the redirect target is never contacted, not merely that an error is returned, because an
error alone is also what a followed-then-rejected redirect produces.

## 429 is retried here, and that is a departure worth naming

The ad-platform clients refuse to retry a non-idempotent request on 429, because without an
idempotency key a retry can double-create a paid campaign. A chat completion has **no durable
side effect**: a retry whose first attempt already reached the model costs tokens, not a
duplicated resource. So retry is unconditional here, bounded at 2 (rather than the siblings' 3)
because tokens are the cost being bounded. `Retry-After` is honoured in both documented forms —
delta-seconds and an HTTP date — and a value beyond `maxRetryWait` **aborts** rather than
sleeping, so a hostile or mistaken large value cannot wedge the client. An absent or unparseable
header means "back off on our own schedule", not "do not retry".

**The 429 body is drained before `Close`.** `net/http` returns a connection to the idle pool
only after its body reaches EOF *and* is closed, so closing an unread 429 makes the retry reopen
TCP and TLS — the slowest possible response to being told to slow down.
`TestComplete_RetryReusesTheConnection` counts distinct client connections the server sees,
which is the only assertion that observes the actual property.

## Deadlines, and why temperature is a pointer

`do` wraps each attempt in `context.WithTimeout` rather than relying on `http.Client.Timeout`,
which an *injected* client does not set — the ordinary case, since injection is for the
transport. `ctx.Err()` is checked **before** each attempt, not inferred from the remaining
budget: a context cancelled without a deadline still reports a full budget, so a caller who has
gone away would otherwise get one more request made on their behalf.

`Config.Temperature` is a `*float64` because a plain `float64` cannot distinguish "the caller
asked for 0" — fully deterministic, which a caller wanting reproducible output will want — from
"the caller said nothing". `Model` and `MaxTokens` need no such treatment: an empty model and a
zero token budget are not meaningful requests.

## Configuration is optional as a group

With `AI_PROXY_URL` or `AI_API_KEY` missing, `NewClient` returns `ErrNotConfigured` at
**construction** rather than per request, so a composition root can decide once — log it and
route callers to their degraded path — instead of every call site rediscovering it.

Present is not the same as usable, so construction also checks that `AI_PROXY_URL` is an
absolute URL with an `http`/`https` scheme and a host, and returns `ErrInvalidProxyURL`
otherwise. `localhost:4000` is the case that makes this necessary rather than tidy: `url.Parse`
accepts it, reading `localhost` as the SCHEME, so nothing objects until `http.NewRequest`
refuses it — on every single generation. A one-line deployment mistake then reads as a
recurring transport failure instead of as misconfiguration, which is the exact outcome
constructing eagerly exists to prevent. `ErrInvalidProxyURL` WRAPS `ErrNotConfigured`
deliberately: to a caller the operational fact is identical — this deployment has no usable
model, degrade — and a separate sentinel would silently stop each of them degrading. It exists
only so the message names the defect. The validated value is used to build the
`/chat/completions` endpoint once, stored on the client, so requests cannot use a form
construction never checked.

### A base url, and nothing more — `ErrProxyURLNotABase`

Construction also rejects a proxy URL carrying **userinfo, a query or a fragment**, with
`ErrProxyURLNotABase` (which wraps `ErrInvalidProxyURL`, so callers still degrade). Two
independent reasons, either sufficient on its own. Mechanically, the endpoint is built by
appending `/chat/completions` to this value: append a path to something ending in `?x=1` or
`#frag` and the result's path is not the one anybody wrote — a value accepted at startup that
then fails, or silently mis-routes, on every generation, which is the very class of late
failure the eager constructor exists to remove. And for disclosure: userinfo and the query are
the two places a credential rides for free inside a URL (`https://user:token@proxy/`, which
Go's transport turns into a Basic credential; `?api-key=…`, a shape several LiteLLM
deployments document). Rejecting is stronger than stripping at print time, because a rejected
value never reaches `Config`, a log, or anything downstream that formats it.

**No rejection path echoes the value.** A URL this constructor is refusing is the one least
entitled to be quoted — it is unvalidated operator input, and one reason it can be invalid is
"there is a token in its userinfo". Each branch names the components it JUDGED instead:
scheme and host, which `url.Parse` has already separated from userinfo and query and which
therefore cannot carry either. The parse-failure branch is the subtle one and the one that
looked most idiomatic before the fix: `url.Parse` returns a `*url.Error` whose `Error()`
embeds the raw url verbatim, so `fmt.Errorf("%w: %w", sentinel, perr)` re-publishes the whole
credential-bearing string.

Unwrapping that to the bare cause — the first fix, and the one that looks sufficient — is
**not** enough, because `net/url`'s causes quote the fragment of the input they choked on.
`https://litellm.internal:sk-secret` has no `@`, so the secret is read as a PORT and the
cause alone is `invalid port ":sk-secret" after host`; `%zz` gives `invalid URL escape
"%zz"`. So the cause is **discarded**, not unwrapped, and the branch returns a message built
entirely from constants. Nothing a caller could use is lost: `url.Parse`'s causes are
unexported types or plain strings, so `errors.Is`/`As` reach nothing, and the sentinel is
what callers match on. `TestNewClient_RejectionNeverEchoesTheRawURL` carries the
malformed-port shape specifically, because it is the case that passes under unwrapping.

**There is no such composition root yet.** This package has no non-test caller: nothing in
`internal/container` imports it, so nothing currently logs `ErrNotConfigured` or degrades
anything. That wiring is part 2 of LFXV2-2775, which adds the email-copy step to the HubSpot
dispatch. This PR ships the client and its contract only. Two of the three `AI_*` values — `AI_PROXY_URL`
and `AI_API_KEY` — are wired in the chart as OPTIONAL secret refs, on the same reasoning as
`SNOWFLAKE_*`: generated copy is an enrichment, so an unprovisioned secret must not stop the pod.
`AI_MODEL` is a plain `value: ''`, because a model id is not a credential and empty is a
meaningful default (it selects `llm.DefaultModel`). See
[internal/infrastructure/config](internal-infrastructure-config.md). The secret is the
**proxy's** key, not a Bedrock or Anthropic credential, so it cannot be replayed against a model
provider directly.

`Config.String()` prints `AIModel` verbatim, masks `AIAPIKey` through `redactSecret` (which
renders `""` for unset and `xxxxx` for present), and reduces `AIProxyURL` through
`redactAIProxyURL` to scheme, host and path — dropping userinfo, query and fragment, and
masking the whole value when it fails to parse or carries a scheme that is neither `http` nor
`https`. The URL is *not* verbatim, and that matters here rather than only in `llm.NewClient`:
`Config.String()` runs on the startup log path **before** the constructor gets to reject
anything, so a pasted credential shaped like a URL reaches a pod log through this function or
not at all. The graded treatment is deliberate: "copy generation did not run" is diagnosed by
knowing whether a proxy and a key are configured at all, and omitting the fields entirely would
be log-safe while answering nothing.

## Three places where a defence has to be exact rather than approximately right

**The bounded read goes one byte PAST the cap.** `io.LimitReader` signals the limit with EOF,
not an error, so `ReadAll(LimitReader(body, cap))` hands back a truncated prefix that is
indistinguishable from a complete body — and the prefix can still parse. A completion object
followed by padding, cut before whatever came after it, unmarshals cleanly and is accepted as
the whole answer. Reading `cap+1` and rejecting when the result exceeds `cap` restores the
distinction between "exactly at the cap" (complete, accepted) and "larger, truncated here"
(rejected). This mirrors the LinkedIn/Meta/Reddit/Twitter clients.

**An overflowing `Retry-After` is over the cap, not absent.** `retryAfter` returns zero for a
header it cannot read, and zero means "back off on our own schedule" — the right answer for
junk, the wrong one for an all-digit delta-seconds value too large for a `time.Duration`. That
value says the proxy's reset is far beyond anything worth waiting for; reported as zero it
sends the caller into ordinary exponential backoff and a retry the proxy has already refused.
Digits are therefore parsed as an integer and compared **in seconds** before any conversion (the
multiply by `time.Second` is itself what wraps), and an overflow is classified above
`maxRetryWait` so `wait > maxRetryWait` aborts. Same shape as the Microsoft sibling. Digits-only
is also the delta-seconds grammar; the previous `ParseDuration(v + "s")` additionally accepted
shapes like `1h2m0` that the header never carries.

**`Temperature` is copied at construction.** It is `Config`'s only pointer field — every other
is a value — so storing `cfg` wholesale left one field aliasing memory the caller still owns.
`Client` documents itself safe for concurrent use, and that claim requires the stored config to
be immutable: a caller reusing its `Config` would otherwise change what later completions send,
and doing so alongside an in-flight `Complete` is a plain data race.

It is **non-streaming, deliberately**: the lfx-one implementation this ports from streams
because its caller is an SSE endpoint rendering tokens into a browser, and this service has no
such caller — generation happens inside a dispatch, one synchronous request with a bounded
budget, and a token stream nobody watches is a slower way to get the same string.

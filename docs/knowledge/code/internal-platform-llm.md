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

`Config.String()` prints `AIProxyURL` and `AIModel` verbatim and masks `AIAPIKey` through
`redactSecret`, which renders `""` for unset and `xxxxx` for present. That asymmetry is
deliberate: "copy generation did not run" is diagnosed by knowing whether a proxy and a key are
configured at all, and omitting the fields entirely would be log-safe while answering nothing.

It is **non-streaming, deliberately**: the lfx-one implementation this ports from streams
because its caller is an SSE endpoint rendering tokens into a browser, and this service has no
such caller — generation happens inside a dispatch, one synchronous request with a bounded
budget, and a token stream nobody watches is a slower way to get the same string.

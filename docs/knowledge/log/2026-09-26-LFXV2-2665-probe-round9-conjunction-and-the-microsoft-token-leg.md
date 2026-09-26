# 2026-09-26 — LFXV2-2665: `ok` is a conjunction, and Microsoft's probe has two legs

**Fix** — Round 9 of the connection-probe work. Three findings, and the two that matter share a
shape: a verdict that was right, defended by a reason that was not.

## `ok` is a conjunction, and the justification said otherwise

The round-8 contract move was correct — an inconclusive probe answers `OK: false`. The
justification shipped with it was not. It read, in the service layer, the design schema and this
bundle, that `ok` is false "because on such a check the credential did not authenticate against
the provider."

That is untrue on the paths that reach the inconclusive arm most often. `googleads`, `microsoft`
and `reddit` reach the platform on TWO legs: a token refresh, then an account read. A refresh that
SUCCEEDS before the account read hits a `429`, a `5xx` or a transport failure means the provider
accepted the stored credential outright — and then the probe lands in an arm whose message told
the operator the credential "was neither accepted nor rejected."

`OK: false` survives the correction untouched, because the field is declared as a CONJUNCTION —
the credential authenticated AND the configured account passed the provider's own check — and an
incomplete check establishes neither half as a whole. What had to change is what the service
CLAIMS: the message may say the verification did not complete, and may not say the credential was
never evaluated.

`TestLinkedinAds` is where the old wording was most plainly false, and the evidence was sitting one
log line above it: reaching that arm REQUIRES the credential baseline to have already passed, so
LinkedIn had demonstrably accepted the credential and only the org-reference cross-check failed to
finish. Its godoc was also still describing the pre-round-8 behaviour outright — `OK: true` with an
advisory — which the sweep missed because it was searching for stale claims about *inconclusive*
and this sentence buried the word in a subordinate clause.

`connection_test.go` and `connection_probe_test.go` each grew an INVERSE guard: the old
"neither accepted nor rejected" phrasing must now be ABSENT, not merely replaced. A test that only
pins the new wording lets the old claim return under a paraphrase.

## Microsoft's `ProbeNotSent` read one marker for a client with two legs

`ProbeNotSent` answered `errors.Is(err, errRequestNotSent)` alone. The five sibling packages all
call `isPreSendDialError`, and Microsoft's omission was documented as deliberate: this client's
REST pre-send arm flattens the cause through `safeCause` into a plain string — so a custom
RoundTripper's text can never reach a persisted campaign step — which also erases the
`*net.OpError` a classifier matches on. That reasoning is sound, and it is why the marker exists
here and in no sibling.

It is also only true of ONE leg, and the doc comment generalised it to the whole client, which is
what made the gap look intended. A fresh probe refreshes before it reads, so the token endpoint is
the first host this client dials and the first that can be unreachable. A dial failure there comes
back as `tokenTransportError`, which renders only `safeCause` but whose `Unwrap` PRESERVES the
cause — `isPreSendDialError` sees through it perfectly well — while `errRequestNotSent` is never
attached, because the REST arm attaches it and the probe never got that far.

So an unresolvable or refused token host answered `false`, and
`Orchestrator.ProbeConnection`'s metrics arm booked an upstream-call sample against Microsoft for a
probe that never left this deployment — charging this network's own fault to Microsoft's error rate
on `campaign_upstream_call_duration_seconds`, which is the single thing the predicate exists to
prevent. It now reads both markers.

`ProbeInconclusive` needed no code change — its `tokenTransportError` arm already answers true —
but its comment carried the same over-generalisation and is now scoped to the leg it describes.

The other five were checked rather than assumed: each wraps its token error in a type whose
`Unwrap` preserves the cause AND already calls `isPreSendDialError`, so both legs were covered
there regardless of which one failed. Microsoft was the sole outlier, and it was the outlier
precisely because it was the one package that had to introduce a marker instead.

Two tests pin the new leg, and both were confirmed to FAIL against the previous predicate before
being kept — a provenance test that passes either way proves nothing. The negative case matters as
much: a mid-flight failure on the token leg (`op: "read"`, not `"dial"`) must NOT be claimed as
never-sent, because the token request may have been received, and dropping its sample would hide a
real Microsoft failure. `isPreSendDialError` matches only DNS failures and dial-op
`ECONNREFUSED`/`EHOSTUNREACH`/`ENETUNREACH`, so a TLS handshake failure — a real conversation with
a real host — stays Microsoft's sample.

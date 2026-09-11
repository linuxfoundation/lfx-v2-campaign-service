# 2026-09-11 — LFXV2-2770: a partial compose is discriminated by its body

**Note** — `compose-audience-master` declares a second error, `ComposePartial`, and Goa maps both
it and `InternalServerError` to HTTP 500, discriminating between them with a `goa-error` response
header. The nearest consumer is the LFX One BFF, which proxies this call and whose error type
carries a body, an original message, a status and a code — **no headers.** So the header is not a
discriminator anyone downstream can actually read.

The bodies are therefore shaped to discriminate themselves: `ComposePartial` carries the created
suppression list alongside `code` and `message`, while `InternalServerError` carries `code` and
`message` only. `internal/service` always passes the address of a value rather than a possibly-nil
pointer, so the field is present whenever the error is, and body-shape discrimination is sound
rather than incidental.

Why declare the error at all instead of letting the failure be a 500: composition creates the
combined suppression list FIRST and the master list second, and that ordering is what makes a
partial failure describable. `ComposePartial` means precisely that the suppression list exists and
the master does not. A caller that treats it as an ordinary 500 offers a retry, and the retry
creates a second suppression list — the duplicate the whole ordering exists to make visible. The
honest affordance is to show the list that WAS created, with a link into the portal, and let the
operator decide.

`ComposePartialError` unwraps to both `ErrComposePartial` and the underlying cause, so a caller
can match the condition without losing what actually failed. The sentinels live in
`internal/audience` beside the transport-neutral results rather than in the orchestration package,
because importing the orchestration would close a cycle through its tests.

Part of [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].

# 2026-09-26 — LFXV2-3282: the Reddit metrics gate's stated reason was the retired one

**Docs** — `EnvRedditMetricsEnabled` in `pkg/constants/constants.go` still justified the gate with
the LFXV2-2995 finding that Reddit published no documentation for its reporting endpoint, and
called the request shape, response shape and spend currency unit a best-effort guess.

That reason is retired. LFXV2-3282 replaced the guess with Reddit's official public OpenAPI spec
and corrected the client against it — `internal/platform/reddit/metrics.go` carries the
`CONTRACT SOURCE` note, and `RedditDispatcher.ReadMetrics` says so at the gate itself. A reader
comparing the two got two different reasons for the same flag, and the constant's was the dead
one.

The gate is unchanged and still correct, for the reason the dispatcher already gave: no request
has ever been made against a live Reddit ad account, and a published schema does not establish
what the endpoint returns for a campaign with no activity, whether `ends_at` includes its final
hour, or whether the account's attribution window shifts the figures. The constant now states
that unknown instead, and points at the dispatcher gate so the two stay in step.

No behaviour change. `docs/api-catalog.md` and the `internal-platform-reddit` concept file were
already current; `pkg/constants` was not the only remaining place describing the superseded state,
as this entry first claimed — see the 2026-09-29 entry that corrects the two concept files it
missed.

Refs: LFXV2-3282

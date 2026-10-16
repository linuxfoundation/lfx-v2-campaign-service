# 2026-10-15 — verify-metrics: a read-only CLI to verify gated metrics contracts

**Creation** — added `cmd/verify-metrics`, the first step of decision D8: an operator runs a
provider's gated campaign metrics read against a live account before flipping
`REDDIT_METRICS_ENABLED` (#167), `MICROSOFT_METRICS_ENABLED` (#168) or `TWITTER_METRICS_ENABLED`.

- Same path as the service: connection repository, system fallback and AES-GCM decrypt, the
  service's dispatcher constructors, and `ReadMetrics`. The flag is forced on in the command's own
  process only.
- Read-only by construction: a READ ONLY database session, a connection reader with no writes, an
  encryptor that refuses `Encrypt`, and an HTTP transport that refuses every request but GET/HEAD
  and the token, Reddit report and Microsoft report submit/poll POSTs before it is sent.
- Secrets are scrubbed from all output (decrypted credential values, auth headers, issued tokens;
  token exchanges withheld; URLs and JSON bodies redacted). `-raw` prints the redacted upstream
  exchanges.
- `google-ads` is refused (not gated; out of scope). Concept:
  [cmd/verify-metrics](../code/cmd-verify-metrics.md).

# 2026-10-07 — Catch a blank-username `:password@host`

**Fix** — review of #276. The punctuation-only username alternative of the scheme-less userinfo
pattern required at least one sub-delim, so a blank username (`:password@host.example`) matched
neither alternative: it stayed verbatim in `config_snapshot` and passed the X tweet screen. Both
patterns (`pkg/redact` and `internal/platform/twitter`) now allow zero sub-delims before the colon.
A blank username is never a clock (`redact.UsernameIsClock` needs digits before the colon), so
`:30@host.example` is caught too. Regression cases are on the X screen, `pkg/redact` and the
dispatch creds suite. See [pkg/redact](../code/pkg-redact.md).

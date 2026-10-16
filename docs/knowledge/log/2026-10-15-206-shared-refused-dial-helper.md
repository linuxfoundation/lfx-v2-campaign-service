# 2026-10-15 — One shared refused-dial test helper

**Fix** — three review threads on #361 (issue #206); tests only, no behaviour change.

- The refused-dial helper (`refusedURL`, `refusedAddr`, `refusedDialTransport`) was copied
  verbatim into five test packages (dispatch, hubspot, meta, reddit, twitter). It now lives once in
  the test-support package `internal/platform/platformtest` as `RefusedURL` and
  `RefusedDialTransport(t)`, imported only from `_test.go` files, so a change to the refusal
  semantics is made in one place.
- The Reddit and X redirecting transports' `deadURL` field is renamed `refusedURL`, matching the
  renamed types.
- Removing the helper's refusal count makes every caller fail its cleanup check, confirming each
  package still routes through it.
- Updated [internal/dispatch](../code/internal-dispatch.md).

# 2026-10-26 — Compose partial-failure text no longer names a combined suppression list (LFXV2-2770)

**Fix** — `composePartialMessage` (`internal/service/audience_explore.go`) still described the
"combined suppression list" compose stopped creating on 2026-10-19. The record-failed shape now
says the master was created and must be attached by hand, that the suppression lists it excludes
are standing lists any repeat reuses, and that composing again can still create a second master: a
master created under its derived name is not found by a repeat, which takes the fingerprinted
name (`resolveComposeNames`). The suppression shapes, unreachable from current code but still in
the contract, drop "combined", and the fallback arm no longer claims a suppression list exists.
Tests: `audience_explore_test.go`, `audience_explore_record_test.go`.

# 2026-10-14 — LFXV2-2665: mark the deparse-probe test DSN as an intentional fake

**Fix** — MegaLinter's secretlint (`@secretlint/secretlint-rule-database-connection-string`)
failed on `internal/infrastructure/postgres/required_index_probe_redaction_test.go:34`, the
deliberately fake `user:password@host` DSN that
`TestProbeWarningsWithholdDSNIdentifiersAndCredentials` (#328) uses to prove the probe's WARN
withholds DSN identifiers. It now carries the repo's existing marker for intentional fake DSNs,
`// secretlint-disable-line -- intentional fake DSN`, as `internal/infrastructure/config/config_test.go`
already does. Test unchanged.

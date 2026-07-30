---
type: "Architecture Doc"
title: "MegaLinter and secret scanning"
description: "How MegaLinter, gitleaks, secretlint, and grype are configured for this repo, including local Docker runs."
resource: ".mega-linter.yml"
---

# MegaLinter and secret scanning

CI runs MegaLinter on pull requests via
[`.github/workflows/mega-linter.yaml`](../../../.github/workflows/mega-linter.yaml)
(Go flavor `v9.1.0`). Repo config is
[`.mega-linter.yml`](../../../.mega-linter.yml).

## Local run

See the **MegaLinter (local)** section in [`README.md`](../../../README.md).
Reports land under `megalinter-reports/` (gitignored).

## Gitleaks

[`.gitleaks.toml`](../../../.gitleaks.toml) extends the default ruleset with
scoped allowlists:

- Test fixtures (`*_test.go`), `go.mod`/`go.sum`, and `CLAUDE.md`
- Documented local/dev AES sample key only in README, the db-conn-check
  quickstart, and `values.local.example.yaml` (path **and** value)

Goa CLI `twitter-api-secret` false positive in
`gen/http/cli/lfx_v2_campaign_service/cli.go` is suppressed via fingerprint
in [`.gitleaksignore`](../../../.gitleaksignore) (not a path allowlist).

Faster secrets-only check:

```sh
gitleaks detect --source . --config .gitleaks.toml
```

## Secretlint

Secretlint runs with MegaLinter's bundled
`/action/lib/.automation/.secretlintrc.json` (preset-recommend). The repo does
not ship a `.secretlintrc.json` or `.secretlintignore`, so MegaLinter falls back
to passing `--secretlintignore .gitignore`.

False positives are suppressed **per line**, with a
`// secretlint-disable-line -- <reason>` trailing comment:

- `internal/dispatch/creds_test.go` — URL with embedded userinfo, asserting the
  snapshot sanitizer fails closed
- `internal/platform/snowflake/client_test.go` (x2) — PEM blocks asserting a
  garbage body and a malformed PKCS8 body error out. Real EC keys in these tests
  are generated at runtime via `ecdsa.GenerateKey`, so nothing is committed.
- `internal/platform/twitter/client_test.go` (x2) — URLs with embedded userinfo,
  asserting they are stripped from UTM output and rejected by validation
- `internal/infrastructure/config/config_test.go` — intentional fake DSN

Prefer line directives over path exclusions here. Unlike the `*_test.go`
allowlist in `.gitleaks.toml`, a secretlint path exclusion would silence
**every** rule for **all current and future** test files — so a real credential
committed to a new test would bypass both scanners. Line-scoped directives keep
new test files fully scanned.

Note that `REPOSITORY_SECRETLINT_FILTER_REGEX_EXCLUDE` has no effect: secretlint
runs in MegaLinter's `project` CLI lint mode, where the filter-regex variables
do not apply.

## Grype

[`.grype.yaml`](../../../.grype.yaml) ignores five known CVEs in
`github.com/docker/docker`, a transitive **test-only** dependency (via
golang-migrate / `dktest`). MegaLinter is pointed at that config with
`REPOSITORY_GRYPE_ARGUMENTS: "--config .grype.yaml"`. Ignores are
package-scoped so new findings in runtime dependencies still fail CI.
Engine patches exist, but a remediated Go module is not yet resolvable on
the path migrate pulls; track a migrate/dktest upgrade separately.

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

## Grype

[`.grype.yaml`](../../../.grype.yaml) ignores five known CVEs in
`github.com/docker/docker`, a transitive **test-only** dependency (via
golang-migrate / `dktest`). MegaLinter is pointed at that config with
`REPOSITORY_GRYPE_ARGUMENTS: "--config .grype.yaml"`. Ignores are
package-scoped so new findings in runtime dependencies still fail CI.
Engine patches exist, but a remediated Go module is not yet resolvable on
the path migrate pulls; track a migrate/dktest upgrade separately.

That ignore list is for **test-only** transitives. A **runtime-scope** CVE is
PATCHED instead, even when the module is indirect — the ignore list must not
grow to cover code that ships. Four such advisories were remediated by version
bump on 2026-07-30 (LFXV2-2811): `google.golang.org/grpc` → 1.82.1 (xDS RBAC /
HTTP2, via `goa.design/clue/debug`), `github.com/apache/thrift` → 0.23.0
(`TFramedTransport` integer overflow, via `gosnowflake` → `arrow-go`), and
`aws-sdk-go-v2/service/s3` → 1.97.3 plus
`aws-sdk-go-v2/aws/protocol/eventstream` → 1.7.8 (EventStream decoder panic /
DoS, via `gosnowflake`). None was added to `.grype.yaml`.

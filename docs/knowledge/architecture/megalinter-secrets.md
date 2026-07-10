---
type: "Architecture Doc"
title: "MegaLinter and secret scanning"
description: "How MegaLinter, gitleaks, and secretlint are configured for this repo, including local Docker runs."
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
- Goa-generated CLI file
  `gen/http/cli/lfx_v2_campaign_service/cli.go` (twitter-api-secret
  false positive; also fingerprinted in `.gitleaksignore`)
- Documented local/dev AES sample key only in README, the db-conn-check
  quickstart, and `values.local.example.yaml` (path **and** value)

Faster secrets-only check:

```sh
gitleaks detect --source . --config .gitleaks.toml
```

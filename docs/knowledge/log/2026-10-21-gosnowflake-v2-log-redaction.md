# 2026-10-21 — move the Snowflake driver to the /v2 module path

**Fix** — `github.com/snowflakedb/gosnowflake v1.19.1` is replaced by
`github.com/snowflakedb/gosnowflake/v2 v2.3.0`, remediating GHSA-qqj6-54q6-cxv6
(Dependabot alert #22): the driver wrote auth tokens, query-result encryption keys,
pre-signed cloud-storage URLs and SAML assertions into its own diagnostic logs.

The alert was nearly dismissed as unfixable, and the reason it looked that way is the
part worth remembering. Its `first_patched_version` is `null`, which reads as "no fix
exists". It is not: Go treats a major version as a **separate module path**, the fix
landed at `v2.2.0` on `.../gosnowflake/v2`, and the unsuffixed v1 path will therefore
never show a patched release — `v1.19.1` is its last. The null is an artifact of the
path split, not a statement about the ecosystem. The advisory's own `vulnerabilities`
array carries both entries side by side and says so plainly; only the alert summary is
ambiguous. Dismissing on the summary would have put a false "no fix available" on the
record for a reachable credential-leak bug.

Reachable, not theoretical: the dependency was a direct `go.mod` require, scope
`runtime`, imported at `internal/platform/snowflake/client.go` and reached in
production through `internal/dispatch/audience_builder.go`'s `ResolvePastEventNames`.
Our connection uses key-pair auth (`sf.AuthTypeJwt`), which is squarely the material
the advisory describes.

The code change is three symbols in one file — `sf.Config`, `sf.AuthTypeJwt`,
`sf.DSN`, all aliases onto `internal/config` in v2 with unchanged shapes — plus the
import path. No test references the driver. What a green build does **not** prove is
the `sql.Open("snowflake", dsn)` driver name: it is a string, it would compile against
either major version, and a changed name would fail only at runtime on the first
query. v2 still registers `"snowflake"` in its `init`, and that seam now has a test:
`TestDefaultOpenerDriverNameIsRegistered` asserts the name is in `sql.Drivers()`. It
is new with this change because the move is what exposed the gap — every existing
test in the package injects a fake opener, so the real driver name was exercised
nowhere, and the import path can change under a string that cannot. The test was
checked against a deliberately wrong name before being kept, which also showed the
registry holds exactly `[snowflake]` — one registration, not two.

Two things were checked that a version bump makes easy to miss. `golang-migrate`
still pulls `gosnowflake@v1.6.19` into the module **graph** for a Snowflake migration
driver this service does not use — but it contributes no package to the binary and
appears in neither `go.mod` nor `go.sum`, so it is neither a surviving copy of the
vulnerable code nor a second `sql.Register("snowflake", ...)`, which would panic at
`init`. `go list -deps ./cmd/...` is the check that answers this; `go list -m all`
prints the graph and would have shown v1 either way. And the transitive bumps `go get`
pulled along were compared against the floors recorded in
`docs/knowledge/architecture/megalinter-secrets.md` for the 2026-07-30 runtime-CVE
remediation: `aws-sdk-go-v2/service/s3` 1.97.3 → 1.106.0 and
`aws/protocol/eventstream` 1.7.8 → 1.7.14, both forward; `apache/thrift` and
`google.golang.org/grpc` unmoved. Nothing regressed below a patched version. That doc
now says those four numbers are a floor to compare against, not a pin to match.

The advisory's second instruction — securely delete retained diagnostic logs — needs
no action here. This service never configures the driver's logger, never sets a log
file, and sets no `GOSNOWFLAKE_*`/`SNOWFLAKE_*` log variable in `internal/`, `cmd/` or
`charts/`; the driver's own `init` leaves the level at `error` and its output goes to
the pod's stream, so there is no log artifact this service wrote and retains.

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
import path. Before this change no test referenced the driver at all; the two added
here are the first, and both import it directly. What a green build does **not** prove is
the `sql.Open("snowflake", dsn)` driver name: it is a string, it would compile against
either major version, and a changed name would fail only at runtime on the first
query. v2 still registers `"snowflake"` in its `init`, and that seam now has a test:
`TestDefaultOpenerDriverNameIsRegistered` asserts the name is in `sql.Drivers()`. It
is new with this change because the move is what exposed the gap — every existing
test in the package injects a fake opener, so the real driver name was exercised
nowhere, and the import path can change under a string that cannot. The test was
checked against a deliberately wrong name before being kept, which also showed the
registry holds exactly `[snowflake]` — one registration, not two.

What the green build and the matching API hid is the one defect this change shipped and
a reviewer caught: **v2 turned OCSP certificate-revocation checking off by default**.
v1's transport skipped OCSP only when `DisableOCSPChecks` or `InsecureMode` was set, so
this service — setting neither — was checking revocation; and v1 did not even need the
field to be set, because `fillMissingConfigParameters` (`dsn.go:714`) promoted the unset
value to `OCSPFailOpenTrue` itself. v2's transport gates the same code on
`OCSPEnabled(cfg)`, which is false for `OCSPFailOpenNotSet` — the zero value. A field
this package had never needed to set now decided the answer, and the answer flipped.
Nothing failed. The fix is one line, `OCSPFailOpen: sf.OCSPFailOpenTrue` — the very
value v1 filled in — with
`TestDSNEnablesOCSPRevocationChecking` asserting it over the built DSN (what `sql.Open`
re-parses), not the config literal. It cannot conflict with v2's CRL mode, which refuses
to run alongside OCSP only when `CertRevocationCheckMode != CertRevocationCheckDisabled`
— and `Disabled` is that field's zero value.

OCSP is the ONLY default that moved, and that is measured rather than assumed: v2's
`FillMissingConfigParameters` was diffed against v1's `fillMissingConfigParameters`
assignment by assignment, and it still fills every one of them — the six timeouts,
`MaxRetryCount`, `Application`, `ValidateDefaultParameters`, `IncludeRetryReason`,
`ProxyProtocol` — dropping exactly one, the `OCSPFailOpen` promotion. So no second
field of `sf.Config` needs setting for parity.

Filled defaults are only half the question, though — OCSP broke at a **gate**, not at a
default, so the gates were diffed too. Every config-driven conditional in v2's
`transport.go` was lined up against v1's: the proxy and `Transporter` branches, the CRL
branch, the OCSP branch, and `validateRevocationConfig`. Two are new in v2 and neither
applies here — a `*http.Transport` type assertion on `Transporter`, and a
`GetTLSConfig(TLSConfigName)` lookup that sits inside the *OCSP-disabled* path, which
this service no longer takes. `validateRevocationConfig` flipped polarity but means the
same thing for these values. And the enabled path is the same symbol on both sides,
`createOCSPTransport`, so the fix puts the connection back on v1's branch rather than on
a new one.

The lesson is about the sweep, not the driver. The pre-push checks confirmed the API
compiled, the CVE floors held and the driver name still resolved — all of which are
questions about *shape*. A major version is precisely where **defaults** move, and a
changed default is invisible to every one of those checks. On a major-version bump,
diff the behavioural gates the old version ran by default against the new one's, field
by field, before trusting a green build.

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

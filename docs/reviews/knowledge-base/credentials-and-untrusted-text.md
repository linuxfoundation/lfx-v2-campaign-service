# Credentials and untrusted text

Both patterns are load-bearing because error strings in this service **do not
stay in memory**. A platform error is recorded into a job's `Steps` narrative and
a campaign's `config_snapshot`, which are persisted unencrypted and reachable
through the API. A leak into an error string is therefore durable, not transient.

---

## platform-error-must-not-carry-untrusted-or-credential-text

**Severity:** `critical`.

**Detect:** In `internal/platform/**` or `internal/infrastructure/**`, either
form:

1. An error built by interpolating an HTTP **response body**, a `Do` /
   RoundTripper cause, a DSN, or an OAuth token-request field — including
   `fmt.Errorf("...: %w", err)` where `err` is a `*url.Error` (which renders the
   full URL, query included).
2. An error-implementing struct with an **exported** field holding such material
   — a `Body string`, an `Err error`, a `Response` — even when `Error()` itself is
   clean. Reflection- and JSON-based logging walks exported fields and bypasses a
   status-only `Error()`, recreating the leak channel the clean `Error()` closed.

**Why it matters:** the material is a bearer token, an OAuth secret, a database
password, or upstream text reflecting request content — and it lands in a
persisted, API-reachable `Steps` entry. Reintroduced with essentially every new
client.

**Evidence:**

- [`r3617520812`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/28#discussion_r3617520812)
  (PR #28): "Wrapping the `pgxpool.ParseConfig` error can expose the original
  connection string (including its password) in the returned message.
  `NewContainer` propagates this error and `main` logs it, so a malformed
  credential-bearing `DATABASE_URL` can leak a secret." Fixed in `85a8a1152`
  ("keep the DSN out of pgx parse-error messages"), which landed with the
  regression test `TestValidateMigrationDSN_ErrorDoesNotLeakSecret`.
- [`r3634987919`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/43#discussion_r3634987919)
  (PR #43) is the exported-field form: "`Body` is exported even though it contains
  untrusted upstream text that may reflect request or credential material.
  Marshaling this error through an `error` interface still serializes exported
  fields, so JSON/reflection-based logging bypasses the status-only `Error()` and
  recreates the leak channel". Fixed in `6c71508`.
- Developer fixing commits on merged PRs: `0f5c218` on **#21**; `7ce9f1d`
  ("redact URLs, ambiguous-outcome handling, robust decode") on **#22**;
  `51b7024` ("stop x/twitter apiError from surfacing body-derived codes") and
  `97d64bc` ("peel every `*url.Error` layer in `safeTransportCause`") on **#31**;
  `dfea35eb7` ("don't leak token-request secrets via a custom RoundTripper
  error") on **#43**.
- Six loci across five merged PRs, 2026-07-14 → 2026-07-24.

**Status on main:** the convention is written into the clients as comments on the
suppression itself. `internal/platform/reddit/client.go:628-635` states:
"Deliberately DO NOT include e.Body: the upstream response body is untrusted and
can reflect request material". `internal/infrastructure/postgres/pool.go:52`
renders a static, DSN-free message and keeps the cause only for `Unwrap`.

**The named helpers split into two kinds, and only one of them redacts.**

| Helper | What it does |
|---|---|
| `safeTransportCause` (`internal/platform/twitter/client.go:549`) | **redacts** — peels every `*url.Error` layer so the embedded request URL never reaches the rendered cause |
| `safeCause` (`internal/platform/hubspot/client.go:360`, and `internal/platform/microsoft/client.go:362`) | **redacts** — same `*url.Error` peeling |
| `truncateErr` / `truncate` (`internal/platform/meta/client.go:1353,1364`) | **length only** — clamps to a rune count without splitting a rune. Strips nothing |

`truncateErr`/`truncate` satisfy no part of this pattern's detect condition on
their own. Applied to an error whose message still embeds a URL or raw upstream
text, the credential-bearing content survives — merely shorter. They belong
**after** a redaction step, never instead of one, so a call site that only
truncates is still a finding.
`docs/knowledge/code/internal-platform-hubspot.md:44-47,53-55` records both the
body-free error rule and the `*url.Error` peeling.

**Not a finding when:** the material is kept for `Unwrap()` only and never
rendered or exported — that is the correct shape. A field carrying a vendor error
*code* or a status is fine; the body is not. The Meta client exports `APIError`
with parsed `Message`/`Type`/`Code`/`FBTraceID` fields deliberately — parsed
vendor fields are not the raw body.

**But `Message` is only parsed vendor data in one of its two branches.**
`internal/platform/meta/client.go:887-890` assigns `env.Error.Message` when
`env.Error != nil && env.Error.Message != ""`; otherwise — a non-Graph or
malformed error body — it falls back to `truncate(strings.TrimSpace(string(raw)),
300)`, which is the **raw HTTP response body**, shortened and unparsed. The
exemption covers the envelope branch only. `Message` reaching a log, error or
response sink from the fallback branch is a genuine instance of this entry's
detect condition, not something already covered.

---

## caller-url-must-be-redacted-before-errors-steps-and-snapshots

**Severity:** `high`.

**Detect:** A caller-supplied URL (`PostURL`, `RegistrationURL`, a click or UTM
URL) reaching an error string, a `Steps` entry, or `config_snapshot` without
passing the package's redactor first. Rejecting userinfo is **not** sufficient —
the query string and fragment must go too. Also flag an unparseable-input
fallback that echoes the input instead of failing closed.

**Why it matters:** these URLs routinely carry `?token=…` style secrets, and
`Steps` and `config_snapshot` are persisted unencrypted and returned by the API.

**Evidence:**

- [`r3575068404`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/21#discussion_r3575068404)
  (PR #21): "The step message echoes the full user-supplied `PostURL`. Even with
  userinfo rejected, query strings/fragments can still contain sensitive tokens
  and will be persisted in `Steps`." Fixed in `10a3239`.
- [`r3575140204`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/21#discussion_r3575140204)
  (PR #21): "`utmURL` preserves every query parameter from `RegistrationURL`, so
  this step can persist and expose credentials such as `?token=...` in the
  campaign result. Keep the full URL only in the Reddit request and redact it
  before adding it to `Steps`."
- Developer fixing commits on merged PRs: `36ad05f` ("reject malformed query;
  never echo unparseable URL creds") on **#21**; `2fcbf44` ("redact PostURL
  query/fragment from `config_snapshot`") on **#36**; plus the URL-redaction work
  on **#20**.
- Five clients plus the dispatch snapshot layer.

**Status on main:** the redactors exist and are named — `sanitizeSnapshotURL`
(`internal/dispatch/creds.go:89`, fail-closed on `@`), `redactURL`
(`internal/platform/meta/client.go:1328`,
`internal/platform/reddit/client.go:1978`), `redactURLForError`
(`internal/platform/twitter/client.go:1068`) and `sanitizePath`
(`internal/platform/reddit/client.go:962`). `config_snapshot` is stored
unencrypted (`internal/domain/model/campaign.go` `ConfigSnapshot`), which is why
`applyCampaignConfig` scrubs before storing.

**Not a finding when:** the full URL is used in the outbound request itself —
that is required. A redactor that keeps scheme, host and path is the intended
output, not a partial fix.

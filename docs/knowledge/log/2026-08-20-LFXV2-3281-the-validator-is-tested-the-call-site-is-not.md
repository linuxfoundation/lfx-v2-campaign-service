# 2026-08-20 — LFXV2-3281 the validator is tested, the call site is not

**Test** — every rule in `internal/service/connection.go` was covered by a test that called the
validator FUNCTION. Nothing tested that a handler CALLS it. Seven of the nine call sites could be
deleted without turning this package red.

## The shape

`validateLinkedInRefreshCredentials` had three dedicated tests covering all-or-none,
supplied-but-empty and padding — thorough about what the rule DECIDES. All three invoke it
directly:

    err := validateLinkedInRefreshCredentials(tc.creds)

Deleting the call from `CreateLinkedinAds` compiles and leaves the whole package green. The
endpoint then persists a partial refresh trio, `CanRefresh()` reads the incomplete set as absent,
and the connection is silently bearer-only — the exact failure the validator's own docstring says
it prevents. The rule was never wrong; it just was not reachable from the endpoint.

A rule that only a test calls is not enforced. The coverage number does not distinguish the two:
the validator's statements are executed either way.

## The enumeration

Both validators, every call site, mutated by deleting the call and running
`go test ./internal/service/`. "Survivor" = the deletion compiled and the package stayed green.

| handler | validator | before | killed by (after) |
| --- | --- | --- | --- |
| `CreateGoogleAds` | slug | killed | `TestCreateConnection_RejectsUUIDProjectID` |
| `CreateRedditAds` | slug | killed | `TestCreateRedditAds_RejectsUUIDProjectID` |
| `CreateLinkedinAds` | slug | **SURVIVOR** | `…AllProviders/linkedin` |
| `CreateMetaAds` | slug | **SURVIVOR** | `…AllProviders/meta` |
| `CreateTwitterAds` | slug | **SURVIVOR** | `…AllProviders/twitter` |
| `CreateMicrosoftAds` | slug | **SURVIVOR** | `…AllProviders/microsoft` |
| `CreateHubspot` | slug | **SURVIVOR** | `…AllProviders/hubspot` |
| `CreateLinkedinAds` | refresh trio | **SURVIVOR** | `TestCreateLinkedinAds_PartialRefreshTrio…` |
| `SetCredentialLinkedinAds` | refresh trio | **SURVIVOR** | `TestSetCredentialLinkedinAds_PartialRefreshTrio…` |

The review reported two sites. The sweep found seven. Two handlers were pinned only because
someone had written a per-provider test naming them — `TestCreateRedditAds_RejectsUUIDProjectID`
even says it is a "spot-check … so the guard isn't accidentally applied to only one provider",
which is the right instinct applied to two of seven providers. Spot-checking a guard that is
copied per provider leaves the un-spot-checked copies free to be deleted.

## Asserting the 400 is the smaller half

The defect is not that the caller misses an error — it is that the row exists anyway. So each new
test asserts BOTH a `*conn.BadRequestError` and that nothing reached the repository, via
`createCalls`/`setCredentialCalls` counters on `fakeRepo`.

The second assertion is separately load-bearing. Moving the guard to AFTER `createConn` — the
caller still gets its 400 — leaves the error assertion satisfied and is caught only by the
counter:

    repo.Create was called 1 times; a rejected payload must never be persisted

A 400 with a silent partial write is worse than no validation, because the operator watches the
request be rejected and the connection is configured wrong regardless.

## Two ways the fixture nearly absorbed the mutation

Both were checked for rather than reasoned about, and one was real.

- **`fakeRepo.SetCredential` returns `ErrNotFound` for a missing row.** Against an empty store an
  unvalidated `SetCredentialLinkedinAds` still returns an error, so a 400-only assertion would
  have passed with the guard deleted — the fixture's own failure standing in for the validator's.
  The test seeds the row first, so the write would SUCCEED and only the guard can produce the
  rejection.
- **A guard that refuses everything** satisfies every rejection assertion. Each case therefore
  carries a narrowing half: the same handler with a canonical slug must be accepted and must
  reach `Create` exactly once.

## What to do with the next validator

Any `if err := validate…(); err != nil` before a persist needs a test that goes through the
handler. The direct-call test stays — it is the cheap place to enumerate the rule's cases — but it
proves the rule, not the wiring, and those fail independently. When the guard is copied across
providers, the handler test is table-driven over ALL of them, so adding a provider and forgetting
the guard fails at the site of the omission.

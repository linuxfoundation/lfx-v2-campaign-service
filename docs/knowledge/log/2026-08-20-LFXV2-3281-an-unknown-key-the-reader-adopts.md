# 2026-08-20 — LFXV2-3281 an unknown credential key the reader silently adopts

**Fix** — the bootstrap installer accepted credential keys no provider contract defines, and
one of them reaches a field that turns a revoked LinkedIn token into a stuck one.

## An unknown key is not an inert key

`canonicalCredentials` folds every supplied key through `credentialKey` and re-marshals the
WHOLE map into the blob it encrypts. Nothing dropped unknown members. That was treated as
harmless on the reasoning that a reader only looks for the fields it declares — which is true,
and is exactly the problem, because the readers are UNTAGGED structs and `encoding/json` then
matches them case-insensitively:

    {"access_token":"tok","access_token_expires_at":"2099-01-02T15:04:05Z"}
      -> folded  {"accesstoken":"tok","accesstokenexpiresat":"2099-01-02T15:04:05Z"}
      -> decoded linkedinCreds.AccessTokenExpiresAt = 2099-01-02 15:04:05 (IsZero=false)

`linkedinCreds.AccessTokenExpiresAt` is a field **no supported write can set**:
`design/connection.go`'s `linkedin-ads-credentials` declares `access_token`, `refresh_token`,
`client_id`, `client_secret` and no expiry. `token.go`'s injected-token branch was documented
"UNREACHABLE IN PRODUCTION TODAY" on precisely that premise. The premise held for the API and
failed for the installer, which writes past the API straight to the repository.

## Why a stuck expiry is worse than no expiry

The branch reuses `c.creds.AccessToken` when the injected expiry is still in the future. Its
own comment already names the coupling that makes this dangerous: `invalidateAccessToken`
clears only the CACHE (`c.accessToken`/`c.tokenExpiry`), never `c.creds`. So after LinkedIn
401s a REVOKED token:

| step | with no injected expiry (today) | with an operator-supplied expiry |
| --- | --- | --- |
| 401 arrives | cache cleared | cache cleared |
| next client (`NewClient` per dispatch) | `c.accessToken == ""` → exchanges refresh material | injected branch fires first → re-serves the SAME rejected token |
| outcome | recovers on the next dispatch | broken until the operator's timestamp passes |

A revocation carries no advance notice, which is the whole reason the 401 arm exists; an
operator-supplied timestamp asserts the opposite and wins. On the LF system row — the fallback
for every project that has connected nothing — that disables LinkedIn service-wide.

## The fix closes the path rather than documenting it

Two shapes were available: teach `invalidateAccessToken` to suppress the injected token, or
refuse the key that makes the state reachable. The second is narrower and was chosen. Making
the injected branch safe would mean making it LIVE — a behaviour change to the refresh path
justified by a value that no supported write produces, to support a field the API contract does
not have. Closing the door leaves the branch dead, and dead by a CHECKED property rather than
by an assumption in a comment.

`requireKnownCredentialKeys` runs on the folded document before any value rule, because the
fault is that the reader adopts the key at all — independent of what it holds. Three details
carry the weight:

- **The allowlist is built through `credentialKey`**, the same fold applied to the input. A
  blocklist of literal spellings would be defeated by `accessTokenExpiresAt`; folding both sides
  makes it a contract check. `refreshToken` stays an accepted spelling of a supported key.
- **Per-provider, required plus optional.** `requiredCredentialKeys[linkedin-ads]` is
  `{"access_token"}` alone, so a required-only list would have made the refresh trio — the
  subject of this PR — uninstallable. `optionalCredentialKeys` carries it, mirroring the
  non-`Required()` attributes in the design.
- **Refused, not filtered.** Dropping the key silently and exiting 0 is the same
  installs-clean-fails-later shape as every other rule here; `requireKnownConfigKeys` already
  refuses unknown `-config` keys for the identical reason.

## A test that was guarding the door open

`TestPaddedCredentialValuesAreRefusedRatherThanStored` asserted that
`{"access_token":"tok","note":"  ignore me  "}` **installs**. It was written to pin something
true and narrow — the padding rule reaches only keys the provider defines — but it also
asserted, as a side effect, that an undefined key is harmless. It passed the entire time the
blob was adopting undefined keys. Rewritten: the surviving case is padding INSIDE a value,
which is what that test is actually about, and the unknown-key case moved to
`TestUnsupportedExpiryCredentialKeyIsRefused` with the opposite expectation.

The counterweight test matters as much as the finding test. A key check that refused everything
beyond the required set would satisfy every rejection assertion and break every provider's
ordinary body, so `TestSupportedCredentialKeysStillInstall` pins bearer-only LinkedIn, the full
refresh trio, Google/Microsoft/X/Meta/Reddit, and the camelCase spelling of a supported key.
The mutation that builds the allowlist WITHOUT folding kills exactly that test and no other.

## Also corrected

`oauthAppFaultCodes`' comment still described its members as the codes describing "the CLIENT
or the REQUEST". After the owner/service split the map holds `invalid_client` and
`unauthorized_client` only — client/application-registration codes whose remedy is an operator
edit — while the REQUEST/PROTOCOL codes live in `oauthRequestFaultCodes`, which documents why
folding them together was wrong. A comment describing a membership the split removed sends the
next reader to add a protocol code back into it.

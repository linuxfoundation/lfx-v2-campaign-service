# 2026-08-07 — LFXV2-2023: the reason a connection is unusable is now a sentinel, because the error text cannot be logged

**Update** — PR #84 gives each stored-connection defect its own domain sentinel alongside
`ErrConnectionNotUsable`, and stops deriving any error text from the decrypted credential blob. This
is the dispatch half of the log-hygiene finding raised on the follow-up PR's discovery handler.

**Fix** — `validateGoogleAdsCredentials` wrapped `json.Unmarshal`'s error verbatim. That error is
computed over the DECRYPTED credential blob and `encoding/json` quotes its input: a
`*json.SyntaxError` reports the offending character, a `*json.UnmarshalTypeError` reports the struct
field it was reading. Wrapping it put credential-derived bytes into every downstream error chain —
and, once the discovery handler logged the cause, into centralized logs — for exactly the connection
whose credentials are malformed. The error is now dropped at the point of detection in favour of a
fixed message. Nothing actionable is lost: the remedy is "re-save the credential", never "fix byte
41".

**Fix** — Dropping the cause would have made the handler's log line useless, since the four
conditions behind a 400 were distinguishable only by their message text. `ErrConnectionInactive`,
`ErrCredentialsUndecodable`, `ErrCredentialsIncomplete` and `ErrProviderConfigInvalid` now carry the
reason as something `errors.Is` reads, wrapped ALONGSIDE `ErrConnectionNotUsable` so the HTTP status
is still decided by the one sentinel. A fixed vocabulary is what a log line wants anyway — it is
greppable and alertable, and it has no payload to carry a secret in.

**Fix** — The first version of the test for this was vacuous, and the way it was vacuous is worth
recording. It planted a distinctive marker in an invalid credential blob and asserted the marker was
absent from the error. It passed against the unfixed code: the blob it used was truncated, and
`encoding/json` answers truncation with "unexpected end of JSON input", which quotes nothing. The
assertion could not fail. `TestGoogleAds_ListAccounts_UnusableReasonsAreClassifiedWithoutPlaintext`
now asserts the undecodable message by EQUALITY against the fixed text, over three blobs covering
both json error types plus a truncation — restoring the wrap fails all three.

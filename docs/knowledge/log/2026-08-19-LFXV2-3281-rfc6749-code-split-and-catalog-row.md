# 2026-08-19 — LFXV2-3281 four OAuth codes carried the wrong remedy

**Fix** — the token-endpoint classification handled one RFC 6749 §5.2 code and swept the rest
into "expired", and the catalog row this PR claimed to have updated was written twice into the
wrong row.

## A binary split cannot express a six-value enum

The previous commit split `invalid_client` out of the expired arm — correctly, and for the
right reason: no member re-authorization repairs a wrong `client_id`. But it left the shape
binary:

    if oauthErrorCode(body) == oauthErrorInvalidClient { ...application fault... }
    // everything else on a 400/401
    return &credentialsExpiredError{...}

RFC 6749 §5.2 defines **six** codes, and the load-bearing claim was checked against the spec
rather than recalled. `invalid_grant` is the one reserved for a dead grant — "The provided
authorization grant is invalid, expired, revoked, does not match the redirection URI used in
the authorization request, or was issued to another client." The remaining four that were
landing on the expired arm describe the CLIENT or the REQUEST:

| code | what it means | can a member re-auth fix it? |
| --- | --- | --- |
| `invalid_request` | malformed request, missing/repeated parameter | no |
| `unauthorized_client` | app not authorized for this grant type (on LinkedIn: not MDP-approved for refresh tokens) | no |
| `unsupported_grant_type` | server does not support this grant type | no |
| `invalid_scope` | requested scope invalid, unknown or malformed | no |

Each was being answered with "re-authorize the LinkedIn connection". That is the same class of
defect the `invalid_client` split was created to fix — a remedy that is **actionable and
provably useless**, which is worse than no remedy: the member repeats an authorization whose
result was never the problem, the failure recurs unchanged, and nobody looks at the app config.

All six are now handled as a closed set, splitting by REMEDY rather than by adding one special
case at a time — the shape the finding was actually correcting. The five non-grant codes take
`ErrApplicationCredentialsInvalid` ("an operator must correct the connection"). That sentinel
is now slightly wider than its name, deliberately: the remedy is identical for all five, and it
is the remedy — not the taxonomy — a caller acts on. Splitting further would add sentinels no
call site could treat differently.

**The fallback is now a choice rather than a leftover.** An absent, non-JSON or unrecognised
code still takes the EXPIRED arm, because that guess fails safely: if wrong, the member
re-authorizes, the real fault resurfaces unchanged, and nothing is destroyed. The opposite
mistake sends an operator auditing a correct app configuration. Worth naming, because before
the split everything unclassified landed there by construction; now it lands there by decision.

### Two tests agreed with the bug

Both used `invalid_request` as the fixture for "an expired refresh token", one of them with an
`error_description` reading "refresh token is invalid, expired or revoked". The description is
never parsed — the CODE classifies — so the fixture and the implementation shared the same
wrong assumption and the tests passed against the defect. Under the corrected split that body
is an application fault, so leaving them would have pinned the wrong remedy. Both were
rewritten to use `invalid_grant`, and the enumerated case list now asserts all five non-grant
codes on both 400 and 401. Reverting to the binary split fails eight subtests and leaves
`invalid_client` passing — which is exactly the blind spot the old shape had.

## The catalog paragraph landed twice in one row and never in the other

`2026-08-19-LFXV2-3281-decode-redaction-and-bootstrap-trio.md` recorded that the catalog
"gained the `credentials_expired` 409 reason on the toggle and metrics rows". It did not.
The paragraph was written into the campaign **toggle** row (`api-catalog.md:139`) TWICE —
626 identical characters, back to back — and the campaign-**metrics** row (:140) still
documented only the previous four LinkedIn reasons.

`ReadMetrics` genuinely surfaces this reason: it routes failures through the same
`linkedinConnectionDefect` / `linkedinExpiry` pair `ToggleStatus` uses, so the metrics row
needed the paragraph on the merits, not merely for symmetry. The duplicate has been deleted
from the toggle row and the paragraph added to the metrics row, extended there with the
distinction this commit introduces: only `invalid_grant` (and an unreadable body) is
`credentials_expired`, while the five client/request codes report
`reason=application_credentials_invalid`.

The earlier fragment was authored by this PR (`4411e5d2`, verified an ancestor of HEAD), so it
is corrected in place with a dated correction note rather than left to assert something false —
another entry's fragment would have been off limits.

## Verified already fixed — no change made

Two suppressed comments named defects that current head no longer has; both were confirmed
against the code rather than assumed from the commit messages:

- `internal/bootstrap/sysacct.go:137` — non-string JSON values treated as absent.
  `validateConditionalGroups` already guards with
  `json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != ""`, so a non-string value fails
  the unmarshal into `string` and counts as absent, which is the requested behaviour.
- `internal/platform/linkedin/token.go:314` — `invalid_client` carrying no sentinel. It returns
  a typed `applicationCredentialsError` whose `Unwrap` yields
  `ErrApplicationCredentialsInvalid`, and the dispatcher re-tags it via `linkedinExpiry`.

Both were addressed in `fff642bf` / `1cbbe0bd`, which the reviewing bot had not seen.

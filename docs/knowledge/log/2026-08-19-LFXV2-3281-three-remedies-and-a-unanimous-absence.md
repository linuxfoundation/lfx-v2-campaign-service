# 2026-08-19 — LFXV2-3281 three remedies, and an absence the guard could not see

**Fix** — the all-or-none credential guard is blind to a UNANIMOUS fault, and the RFC 6749
correction that landed earlier tonight over-corrected: it moved three protocol codes onto a
remedy no operator can apply.

## A guard cannot see a fault that is total

`validateConditionalGroups` enforces the LinkedIn refresh trio as all-or-none. Its shape was:

    var v string
    raw, found := folded[credentialKey(want)]
    if found && json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" {
        present = append(present, want)
        continue
    }
    absent = append(absent, want)
    ...
    if len(present) == 0 || len(absent) == 0 { return nil }

The comment on it claimed a non-string value "counts as absent rather than satisfying the
group". True, and incomplete in the way that mattered. Trace three non-string values —
`{"refresh_token":1,"client_id":2,"client_secret":3}`:

| member | unmarshal into `string` | bucket |
| --- | --- | --- |
| `refresh_token` | fails | `absent` |
| `client_id` | fails | `absent` |
| `client_secret` | fails | `absent` |

`present == 0`, so the guard returns nil, `canonicalCredentials` marshals the folded blob and
the installer writes it to the SYSTEM row at exit 0. The mutation run makes it concrete — the
reverted code produced a `created` row whose `EncryptedCredentials` decode to
`{"accesstoken":"tok","clientid":2,"clientsecret":3,"refreshtoken":1}`. Dispatch cannot decode
that into `linkedinCreds`, so every project falling back to the LF row fails, far from the
operator who typed it.

**This is the boundary combination a guard misses precisely because it is uniform.** One bad
member out of three leaves `present=2, absent=1` and the all-or-none arm fires — for the wrong
reason, but it fires. A test written with one bad field and two good ones therefore passes
against the bug. Only totality reaches the hole.

The fix distinguishes three outcomes where there were two:

- **genuinely omitted** (key not in the document) → `absent`. Legitimate: LinkedIn issues
  refresh tokens only to MDP-approved partners, so a bearer-only system row is the common case
  and must keep installing.
- **present but not a non-empty string** → `mistyped`, refused with its own message naming the
  offending keys.
- **a usable string** → `present`, with the existing padding check unchanged.

Malformed members are deliberately NOT flipped into `present`. That would refuse the install —
correct outcome — with the all-or-none message, reporting `"client_id": 123` as "supplied
refresh_token, client_secret but missing client_id" and sending an operator to look for a field
they did supply. A type fault gets a type fault's message.

The required-key loop above was swept for the same shape. It **cannot leak**: it has no
`return nil` escape hatch, every outcome there is fatal. But it collapsed omitted, non-string
and blank into one "credentials are missing" string, which names the wrong correction for two
of the three, so it now separates them for the message alone.

## Not-the-member's-fault does not imply the-operator's-fault

The previous commit split the six §5.2 codes two ways: `invalid_grant` → expired,
everything else → `ErrApplicationCredentialsInvalid`. The negative claim was right — no member
re-authorization repairs `invalid_request`. The positive claim was assumed.

`ErrApplicationCredentialsInvalid` resolves to a connection-repair 409 telling an operator that
their stored `client_id`/`client_secret` are wrong. For three of the five codes that is false
in a checkable way: `invalid_request` (malformed/missing/repeated parameter),
`unsupported_grant_type` (the server does not implement this grant) and `invalid_scope` describe
the REQUEST or the PROTOCOL. LinkedIn never evaluated either credential. Editing a credential
cannot make a malformed refresh request well-formed.

`invalid_scope` is the cleanest disproof: **this client sends no `scope` parameter at all** on a
`refresh_token` grant. There is no scope field on a connection for anyone to fix.

So the enum splits by WHO CAN REPAIR IT, which is the only thing a caller acts on:

| §5.2 code | sentinel | reason token | remedy |
| --- | --- | --- | --- |
| `invalid_grant` | `ErrCredentialsExpired` | `credentials_expired` | the MEMBER re-authorizes |
| `invalid_client`, `unauthorized_client` | `ErrApplicationCredentialsInvalid` | `application_credentials_invalid` | an OPERATOR edits the connection |
| `invalid_request`, `unsupported_grant_type`, `invalid_scope` | `ErrTokenRequestRejected` | `token_request_rejected` | WE fix the service |

`domain.ErrTokenRequestRejected` is the first reason in this vocabulary that points at the
service rather than at the caller's configuration. It is still wrapped alongside
`ErrConnectionNotUsable` — the fault is permanent, so it must not land on the retryable 503 —
but its message says "this is a defect in this service, not in the stored connection", and a
test asserts the message carries neither credential remedy. The sentinel alone is not enough:
an `ErrTokenRequestRejected` whose text told someone to correct their credentials would
reproduce the defect while every `errors.Is` assertion stayed green.

The previous entry defended the wide sentinel: "the remedy is identical for all five, and it is
the remedy — not the taxonomy — that the caller acts on." The premise was the error. The remedy
is not identical for all five; it has a different OWNER for three of them.

**Both halves of the dispatch wiring had to move.** `linkedinExpiry` re-tags the new sentinel
and `linkedinConnectionDefect` recognises it. A sentinel added to the first but missed by the
second is classified correctly and then never reached, because the predicate is what the call
sites guard on — the exact shape of the original `invalid_client` bug. The mutation confirms
it: disabling only the predicate arm strands the error on the generic retryable path.

## A test that agreed with the over-correction

`TestNonGrantOAuthCodesAreApplicationFaults` asserted `ErrApplicationCredentialsInvalid` for all
five non-grant codes, so it passed against the defect: the fixture and the implementation shared
the assumption that "not a dead grant" means "an operator's fault". It is rewritten as
`TestNonGrantOAuthCodesSplitByWhoCanRepairThem`, a table that asserts, for each code, the one
sentinel it MUST carry and the two it must NOT — a positive-only assertion passes against a
classifier that returns everything.

## The toggle row published a narrower vocabulary than it emits

`docs/api-catalog.md:139` documented four LinkedIn 409 reasons plus `credentials_expired`, and
stopped. But `ToggleStatus` routes every token-exchange defect through the same
`linkedinConnectionDefect` / `linkedinExpiry` pair `ReadMetrics` uses, so the two rows describe
one implementation — and the metrics row directly below already documented
`application_credentials_invalid`. Two paths, same code, different published vocabularies.

The toggle row now carries both `application_credentials_invalid` and the new
`token_request_rejected`, and the metrics row's paragraph was re-cut along the three-way split.
A run-on left by the previous edit (`...never reads the account**An account mismatch`, with no
sentence terminator) is closed while the row is open.

## Verified NOT fixed — a claim this bundle got wrong

`2026-08-19-LFXV2-3281-rfc6749-code-split-and-catalog-row.md` closes with a "Verified already
fixed — no change made" section asserting that `internal/bootstrap/sysacct.go:137` already
handled non-string values correctly, quoting the very guard analysed above. That reading is
wrong: it checks what happens to ONE malformed member and never traces the case where all three
are. The finding had been raised three times and closed three times on that reasoning. It is
recorded here rather than corrected there, because another entry's fragment is off limits.

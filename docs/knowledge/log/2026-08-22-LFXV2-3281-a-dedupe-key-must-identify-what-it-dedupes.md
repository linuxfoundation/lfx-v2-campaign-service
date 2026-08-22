# 2026-08-22 — a dedupe key must identify the thing it dedupes

**Fix** — the LinkedIn refresh-token near-expiry warning is deduped once per process per
connection. Its key was `ConnectionLabel + ClientID + DefaultAccountID`, and none of those three
identifies a connection.

- `ConnectionLabel` is operator-set and OPTIONAL. Every connection whose operator set no name
  falls back to the shared constant `"the LinkedIn connection"`.
- `ClientID` is the OAuth APPLICATION id. Every connection on one Marketing Developer Platform
  app carries the same value — that is what an application id is.
- `DefaultAccountID` is EMPTY on the discovery path: `ListAccounts` builds the client with a
  zero `RuntimeConfig` deliberately, because the finder asks what the TOKEN reaches and scoping
  it to one answer would narrow the question.

So two unnamed connections on one app produced an identical key while holding different refresh
tokens with different expiry dates. The first to warn silenced the other for the life of the
process, and the silenced credential expires with no notice — the exact outcome the warning
exists to prevent. The same key also failed in the opposite direction: one connection's key
CHANGED once an account was selected, so it warned twice for one credential.

The key is now the connection ROW id (`resolved.connID`), which is one value per connection and
changes for neither reason. It is not a secret — it names a row — and it is never logged.

## The half that would not have bound

Adding `Credentials.ConnectionID` and rekeying makes the platform-package test pass on its own,
and in production nothing would have changed: all four LinkedIn client constructions go through
`linkedinCredentials`, which did not carry the field, so `refreshExpiryWarnKey` would have taken
the fallback branch every time while the new test stayed green.

`TestLinkedinCredentialsCarriesTheConnectionRowID` pins the dispatcher seam for that reason, and
it is the one that fails when `linkedinConnID` is stubbed to `""` — a mutation the
platform-package test cannot see at all. A fix that spans two packages needs a test on the SEAM,
not only on the end that was reported.

## What the reviewers got right and wrong

Both bots reported this, and the mechanism they described held up when checked against the code:
the shared fallback label, the shared application id, and the empty account id on discovery are
all real, and `ListAccounts`'s zero `RuntimeConfig` is right there in the source. The remedy they
proposed — thread an immutable non-secret connection identifier and keep the label for the log
message — is what was implemented, and `resolved.connID` already existed for the credential
cache, so nothing new had to be invented to carry it.

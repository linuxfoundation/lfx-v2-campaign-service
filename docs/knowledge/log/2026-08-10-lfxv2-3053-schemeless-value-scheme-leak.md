# 2026-08-10 — LFXV2-3053: a query's `://` was read as a schemeless value's scheme

**Fix** — `redactOne` located the authority with `strings.Index(u, "://")`, which finds the
first `://` **anywhere in the value**. For a value carrying no scheme of its own, that is the
one inside its own query, so `authStart` landed past the query and the search for userinfo
began after the credential.

Confirmed before changing anything, by calling the exported function:

| input | printed |
|---|---|
| `u:p@host?redirect=https://x` | `u:p@host` |
| `user:s3cret@idp.example/jwks?redirect=https://x` | `user:s3cret@idp.example/jwks` |
| `u:p@host,v:q@host2?redirect=https://x` | `u:p@host,v:q@host2` |

## Why this leaked rather than merely losing the host

Every other authority-bound defect in this package printed *less* than it should. This one
printed the credential, and the reason is worth keeping: a value whose `@` sits **before** the
misplaced `authStart` has no `@` **after** it either. So it fails the userinfo search, fails the
"an `@` past the query" refusal that exists to catch exactly this ambiguity, and falls through
to `trimQueryAndFragment` — which, with `authStart` already past the `?`, has nothing left to
cut. Three guards in a row are skipped by the same wrong offset.

The pathful case is the worst of the three, because the output is *plausible*: host and path
survive, as they are supposed to, and the credential sits at the front of a URL that looks
correctly redacted.

## The rule, and where it already existed

`allSchemed` has required a valid scheme at the **start** of a segment since an earlier round —
`i > 0 && isScheme(p[:i])`. `redactOne` accepted a `://` anywhere. That disagreement is the
defect in one sentence: a value the list rule rejects as schemeless is then handed to the
function that treats it as schemed. Both now go through `schemeEnd`.

Schemeless input is not hypothetical. `allSchemed` declines to split the schemeless NATS list
form deliberately, and `URLUserinfo` exists partly for the path where `url.Parse` **failed** —
the error embeds the raw URL, and a malformed value is exactly where a missing scheme comes
from.

## Regression guard

Four cases in `TestURLUserinfo_NeverEmitsACredential`. Reverting `schemeEnd` to the old
`i >= 0` fails the first three by name, each diagnostic naming the credential that escaped. The
fourth — `https://user:s3cret@idp.example/jwks` → `https://***@idp.example/jwks` — passes under
both, on purpose: it is the guard against over-correcting into "no value has a scheme", which
would redact every schemed URL down to a form with no authority. Over-rejection would be a
defect here too.

## Verification

`gofmt -l`, `go build ./...`, `go vet ./...`, `golangci-lint run ./pkg/redact/...` clean;
`go test -race -count=1 ./pkg/redact/` green.

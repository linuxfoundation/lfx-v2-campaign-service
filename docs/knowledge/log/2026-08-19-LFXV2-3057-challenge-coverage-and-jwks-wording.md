# 2026-08-19 — LFXV2-3057 challenge coverage, and the JWKS claim was wrong

Three review findings on the 401 change, all verified before acting.

## The brief/audience challenge header had no test behind it

`TestEveryConnectionUnauthorizedEncoderSetsTheChallenge` read one generated file, the
connections encoders. But briefs and audiences take their transport mapping from
`briefErrorResponses` (`design/brief.go`), which is a SEPARATE `Header()` call from the
connections one in `connectionAuthErrorResponses`. Deleting it would leave every brief and
audience 401 a bare status with no `WWW-Authenticate` challenge — an RFC 9110 §15.5.2
violation — and no test would have noticed.

The service-layer tests cannot cover this, and the reason is worth recording: they assert
`UnauthorizedError.WwwAuthenticate`, the STRUCT FIELD, which the handler populates before
transport. That field stays correct while the header vanishes from the wire. Only the
generated encoder decides whether the value becomes a header or serializes into the JSON
body.

Verified by mutation rather than argued: removing the `Header("www_authenticate:WWW-Authenticate")`
call from `briefErrorResponses` and regenerating dropped the challenge from all 22
brief/audience `Unauthorized` arms. Under that mutation the FULL `internal/service` suite
reported exactly one failure — the newly generalised test:

```
briefs/server/encode_decode.go:   0 encoders set the WWW-Authenticate challenge but 17 have an Unauthorized case
audiences/server/encode_decode.go: 0 encoders set the WWW-Authenticate challenge but  5 have an Unauthorized case
```

That single-failure result is the finding: before this change the same mutation was silent.
The test now iterates every generated server encoder that carries a 401.

## The JWKS-outage claim contradicted the implementation

Both the knowledge concept and the log entry cited "an expired credential or a JWKS outage"
as what the old 400 mis-alerted on. That is false for the JWKS half, and each document
contradicted its own 503 section a few paragraphs later.

`authenticate` has always routed `domain.ErrKeyUnavailable` to the `unavailable` branch
(`auth.go:105-109`), which answers 503. Only token-side refusals ever returned 400, and only
those moved to 401. Answering 401 for a JWKS outage would tell a caller to refresh a
credential that was never the problem — which is exactly why the split exists.

Both sites now scope the alerting consequence to refused credentials and point at the 503
note rather than cutting across it.

## design.md pointed at a deleted file

The knowledge concept cited `internal/service/connection_badrequest_encoder_test.go`, which
this change deleted, and described only the BadRequest assertion. Updated to
`connection_auth_encoder_test.go`, noting that the test requires a `case` for both
`"BadRequest"` and `"Unauthorized"`, and documenting the challenge test alongside it.

**Verification** — the mutation above (design `Header` call removed + `goa gen`) fails the
generalised test on both brief and audience encoders and passes everything else; reverting
the design and regenerating restores a clean `gen/` tree with no diff, confirming the
committed generated code matches the design.

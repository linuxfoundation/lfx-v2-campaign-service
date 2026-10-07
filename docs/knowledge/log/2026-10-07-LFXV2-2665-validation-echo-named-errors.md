# 2026-10-07 — LFXV2-2665 keep named error bodies; sanitize only validation errors

**Fix** — d206a88ba passed `nonEchoingErrorFormatter` as the formatter to all five generated
Goa servers so decoder-validation 400s stopped echoing the rejected value. That regressed every
NAMED error: each generated `Encode<Method>Error` hands a named error to a non-nil formatter
instead of building its generated response body, and the named error types' `Error()` returns
`""`. A Microsoft campaign-ref `BadRequest` that should have answered
`{"code":"400","message":"platform_campaign_id must be the numeric Microsoft Ads campaign id"}`
answered `{"name":"fault","id":"…","message":"","temporary":false,"timeout":false,"fault":true}`
with status 400; conflicts lost their machine-readable `reason` the same way (adopt-campaign 409,
account-monitor 409), and so did every 404 and 503.

The generated servers are back on a nil formatter, so named errors encode exactly their generated
bodies, and are built with `nonEchoingResponseEncoder` (`cmd/campaign-service/error_formatter.go`)
instead of `goahttp.ResponseEncoder`. It rewrites only a `*goahttp.ErrorResponse` — the type
Goa's default error path renders, never a named-error body or a result — whose name is a
decoder-validation name, part by part: the message is split on `"; "` outside Go-quoted strings,
a value-echoing part becomes `<field> <fixed sentence>`, a missing-field/payload part is kept, and
an unrecognized part becomes `a field failed validation`. Nothing is buffered and the
`ResponseWriter` is not wrapped. New `named_error_body_test.go` pins byte-exact generated bodies
for `BadRequest`, both `Conflict` shapes with `reason`, `NotFound` and two `ServiceUnavailable`s
through the real mux; it fails with any non-nil formatter, and the 30-case echo test fails with
the plain encoder. See [cmd/campaign-service](../code/cmd-campaign-service.md).

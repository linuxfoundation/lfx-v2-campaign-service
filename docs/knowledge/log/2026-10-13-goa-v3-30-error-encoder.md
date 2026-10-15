# 2026-10-13 — Bump goa to v3.34.0, removing the accepted ErrorEncoder race

**Update** — `goa.design/goa/v3` (go.mod and the Makefile's `GOA_VERSION`) moves from v3.25.3
to v3.34.0 and `gen/` is regenerated. goa v3.29 resolves `goahttp.ErrorEncoder`'s nil-formatter
default outside the returned closure, so the race recorded as accepted on 2026-10-07 is gone with
the nil formatter the generated servers must keep. New `error_encoder_race_test.go` drives
concurrent non-named errors through one endpoint of the real mux; it reports the race under
`-race` on v3.25.3 and passes on v3.34.0. goa v3.34.0 requires Go 1.26, so the `go` directive
moves to 1.26.0 (CI reads it from go.mod). Generated-code changes adapted here: a `Tag`ged
response no longer renders the tag attribute in an implicit body, so `upload-creative-asset`'s 201
arm declares `Body(CreativeAsset)` to keep `created`; `invalid_length` messages and nested
missing-field contexts changed shape, and `nonEchoingResponseEncoder` recognizes both. Nested
validation messages now name the full body path (`body.credentials`, `body.config.account_id`).
The generated CLI `Build*Payload` helpers now take `*string` flags (one test adapted).
OpenAPI output dedupes schemas and corrects the ComposePartial 500 schema; no operation, status
or wire shape changed. goa now also emits `gen/goa.json` and `gen/http/openapi3.2.{json,yaml}`.
See [cmd/campaign-service](../code/cmd-campaign-service.md) and [design](../code/design.md).

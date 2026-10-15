# 2026-10-15 — #183: the generated upload client declares its Content-Length

**Fix** — Creative-asset uploads from the generated briefs client always serialized. goa's stock
`RequestEncoder` never sets `ContentLength`, so the transport sent the upload chunked, the server
saw `-1`, and `UploadAdmissionWeightFor(-1)` charged the whole admission budget: concurrency 1 for
a 4 KiB logo. As agreed on #170 and recorded on #183, the fix is on the client and the server's
worst-case price for an unknown length is unchanged.

New [pkg/uploadclient](../code/pkg-uploadclient.md): `RequestEncoder` is a drop-in for
`goahttp.RequestEncoder` (same JSON bytes) that also sets `ContentLength` and `GetBody`. The
regression tests in `cmd/campaign-service/upload_client_length_test.go` drive the real generated
client against the real `buildHandler` chain: with the new encoder the server observes a positive
length and two small uploads are admitted together; with the stock encoder the server observes
`-1` and a second concurrent upload is shed with `503`. Removing the encoder's `ContentLength`
assignment fails the concurrency test.

[internal/middleware](../code/internal-middleware.md), `pkg/constants` and `docs/api-catalog.md`
now point at the encoder instead of describing chunked as the generated client's only behaviour.

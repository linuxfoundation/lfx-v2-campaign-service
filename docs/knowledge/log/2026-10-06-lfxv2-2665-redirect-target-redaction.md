# 2026-10-06 — LFXV2-2665 Redirect-target redaction and creative test-goroutine hygiene

**Fix** — The creative image fetch's redirect refusal rendered the redirect TARGET
with `url.URL.Redacted()`, which masks only a password in userinfo and keeps the
query verbatim. A redirect to a signed CDN asset therefore put its signature into the
refusal message, which `net/http` wraps into the `*url.Error` that becomes a persisted
Steps entry. The round-one sweep closed the thirteen sites that render the caller's
OWN URL and missed this one — the redirect target is the same class of secret reached
by one hop, and the site reads as already-handled precisely because `Redacted()` is in
its name. It now routes through `redactURLForError` like every other site, keeping
scheme+host+path so the refusal still names the host it refused to follow.

**Fix** — Three handlers in `demandgen_creative_test.go` rendered their fixture PNG
inside the `httptest` handler, so `pngOf`'s `t.Fatalf` could reach `FailNow` from a
non-test goroutine — where it does not stop the test and can leave the server blocked
while a deferred `Close` runs. The bytes are now rendered on the test goroutine before
the server is built. `TestFetchOneImage_SendsNoCredentials` additionally captured the
request headers — the value its assertion depends on — with no happens-before edge
between the handler goroutine and the test; the capture is now mutex-guarded and read
under the same lock, matching `capturedMutate` in `geo_test.go`. See
`docs/reviews/knowledge-base/test-hygiene.md`,
`httptest-handler-state-needs-synchronized-handoff`.

**Docs** — `docs/api-catalog.md` told a consumer to `GET` the account's conversion
actions. `ListConversionActions` is a Go client method with no Goa method, no mount and
no catalog row; the line now says so plainly, following the catalog's own precedent for
a designed-but-unbuilt read, so the `conversionActions` field no longer implies an HTTP
route that does not exist.

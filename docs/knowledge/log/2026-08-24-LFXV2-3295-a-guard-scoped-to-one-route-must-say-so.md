# 2026-08-24 — a guard scoped to one route must say what it does not cover

**Docs** — `UploadAdmission`'s doc block enumerates, carefully, what its weight does NOT account
for: the decoded pixel buffer (split out to `DecodeReserver`), and dispatch-side memory read back
from Postgres. Both entries exist because the block opens by claiming pre-auth memory protection,
and a bound that silently under-counts "reads as protection while providing less than it appears
to" — its own words.

The enumeration was missing an entry. `isUploadRequest` gates the middleware to the creative-asset
POST, so **every other body-bearing route decodes with no permit at all**, bounded only by
`MaxBodyBytes`' per-request 42 MiB cap.

## The retention is real, not theoretical

The tempting dismissal is that other routes declare small fields — the largest non-upload string
in `design/` is `MaxLength(8000)` — so an oversized body is rejected anyway. That reasoning
inverts the order of operations. `encoding/json` materialises the string into the struct *before*
the generated validator runs. Measured, not argued:

```
RETAINED name length = 41943040 bytes before any MaxLength check
```

And decoding precedes authentication on those routes too:
`gen/http/.../server/server.go:204` calls `decodeRequest(r)` before the endpoint (and its
`authJWTFn`) is invoked. So a route whose contract admits 8000 characters can hold ~42 MiB from
one unauthenticated request, and N of them concurrently take no semaphore permit.

## Why the fix is a comment, not more middleware

Recorded as a deliberate scope boundary rather than closed, because the cheaper lever is
elsewhere. The exposure on other routes is **the cap being generous, not the permit being
absent**: `MaxRequestBodyBytes` is 42 MiB globally only because the upload's contract needs it,
and no other endpoint takes a body remotely that large. Extending admission to every route means
inventing route-specific caps and weights for endpoints that should simply never see a 42 MiB
body.

So the doc now names the gap and points the next person at the right lever — make
`MaxRequestBodyBytes` per-route, or lower it globally and raise it only for the upload — rather
than widening a control built for the one route whose contract admits a body three orders of
magnitude larger than any other.

## The rule

**A guard that names its own limits must enumerate every exit from its scope, including the
routes it never runs on.** This block had two "does NOT account" entries and read as exhaustive;
the missing third was the one an attacker reaches by changing the path. An enumeration that is
almost complete is more dangerous than none, because it is read as a survey.

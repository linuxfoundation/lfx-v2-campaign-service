# 2026-09-28 — LFXV2-2665: row-decode errors carried upstream-derived text

**Fix** — round 20 of review, raised by the learnings reviewer as a match on
`docs/reviews/knowledge-base/credentials-and-untrusted-text.md`,
`platform-error-must-not-carry-untrusted-or-credential-text` (severity `critical`).

Both Google Ads row decoders wrapped the `json.Unmarshal` cause:
`fmt.Errorf("decode customer row: %w", uerr)` in `selfReach`, added in round 19, and the same
shape in `queryCustomerClients`, which predates it. The KB entry forbids both forms this takes
at once. The cause is derived from an HTTP **response body** — a `json.UnmarshalTypeError`
renders the offending value — and it is stored in `transportError.Err`, an **exported** field,
so reflection- and JSON-based logging walks it regardless of what `Error()` renders. These
errors reach the persisted, API-reachable `Steps` narrative, which is what makes the leak
durable rather than transient.

Both sites now return fixed package-level errors, `errDecodeCustomerRow` and
`errDecodeCustomerClientRow`. Nothing this layer can act on is lost: the row failed to decode,
`transportError` still carries the method and path — both composed here, neither upstream — and
the probe classifies an unrecognised error as inconclusive either way, which is the same
verdict the wrapped form produced.

The pre-existing `queryCustomerClients` site was fixed alongside the new one rather than left
for later. It is the identical hazard in the same function family, and leaving it would mean the
next reader finds two decoders spelling the rule two ways and copies whichever they meet first.

Also in this round, `probe_configured_customer_test.go` stopped naming a Microsoft error code in
its fixture bodies. Those bodies previously carried `{"Code":1100,"ErrorCode":"CustomerNotFound"}`,
which is not a code this repo has pinned against the live API. The predicate gates on the status
and the id's provenance and never reads a code — see
`2026-09-28-LFXV2-2665-microsoft-configured-customer-400-paged-instead-of-answering.md` for why —
so a fixture asserting one claimed knowledge the fix deliberately does not have, and would have
let a future code-based narrowing look tested when it was not.

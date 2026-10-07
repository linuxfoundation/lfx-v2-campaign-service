# 2026-10-06 — LFXV2-2665 Creative scheme echo, the total-bytes cap in the catalog, and PMax test hygiene

**Fix** — `validateImageURLs` refused a non-https creative image URL with
`got scheme %q`, echoing the caller's scheme back. At that point the code has
established only that the scheme is *not* `https` — nothing about what it actually
is — and a bare token parses as a scheme all on its own (`"sk-secret:foo"` has scheme
`"sk-secret"`), so the echo is one more place a credential can land in an error that
persists unencrypted as a Steps entry. The refusal now names the required scheme from
our own constant and drops the echo, which costs the caller nothing: they already know
what they sent. The site is shared by Demand Gen and Performance Max, so one change
covers both channels. See `docs/reviews/knowledge-base/credentials-and-untrusted-text.md`,
`caller-url-must-be-redacted-before-errors-steps-and-snapshots`.

**Fix** — Two handlers in `pmax_test.go` built their fixture images inside the
`httptest` handler, so `pngOf`'s `t.Fatalf` could reach `FailNow` from a non-test
goroutine. The round-one sweep fixed exactly this shape in `demandgen_creative_test.go`
and did not carry it to the sibling file added by the same branch; both sites now
render on the test goroutine before the server is built. See
`docs/reviews/knowledge-base/test-hygiene.md`,
`httptest-handler-state-needs-synchronized-handoff`.

**Docs** — `docs/api-catalog.md` documented the 5 MiB per-image cap on
`demandGenCreative` and `performanceMaxCreative` but not the 64 MiB cap on the sum of
every image in one creative, so a consumer whose images each passed every per-image
bound had no way to anticipate the refusal. Both entries now state the total alongside
the per-image cap and say plainly that the two are separate refusals, matching the
catalog's existing treatment of the Meta surface's distinct-asset bound.

**Update** — Added `TestFetchSlotImages_TotalBytesCapIsAcrossEverySlot`. The aggregate
cap is the only bound in the creative path that is stateful across iterations — every
other check is a pure function of one input, already covered by table tests. A running
total is the kind of check that survives a refactor syntactically and dies
semantically, so the test spans two slots to pin that the total is across the whole
creative rather than per slot, and asserts both the refusal above the sum and
acceptance one image below it.

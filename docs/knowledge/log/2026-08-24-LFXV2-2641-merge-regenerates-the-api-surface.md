# 2026-08-24 — LFXV2-2641 the merge conflicted only in generated code

**Update** — merging `origin/main` brought LFXV2-3281's LinkedIn refresh attributes into
`design/connection.go`, which this branch also edits to add the Google Ads keyword and
audience reads. The design file itself auto-merged: the two changes touch different types,
and the merged file carries thirteen distinct `Method(...)` names with no duplicate and no
`…Endpoint2` — checked by reading the result, because a `design/` file in this repo has
auto-merged into duplicate Goa methods before, and duplicates compile.

All twelve conflicts were in DERIVED files: the four `gen/http/openapi*` specs, their four
`cmd/campaign-service/kodata/gen/http/` copies, and four generated CLI files. None of them
is edited by hand, so resolving them by choosing a side is meaningless — the correct
resolution is to take either side to clear the index and then re-derive the whole surface
from the merged design with `make apigen`.

Taking ours ALONE does not build: the kept specs describe this branch's routes while the
merged design also declares main's, so the generated server and the spec disagree.

**Verification** — the regenerated `openapi3.json` contains both sides' contributions:
main's `MDP-approved` refresh-token description and this branch's
`get-google-ads-keywords`. Path-set arithmetic confirms a true union rather than a
side-pick — ours 44, main 41, merged 44, with nothing missing from the union and nothing
present that neither side declared.

`make apigen` copies the four specs into `cmd/campaign-service/kodata/gen/http/` as its
last step, and all four compare byte-identical to their `gen/http/` originals. That pair
is what the deployed pod serves, and a divergence there is not caught by CI.

The tenancy fix this branch carries is untouched by the merge: `googleAdsScopeForCustomer`
still gates both the keyword and audience reads, and the merge's only edits to
`internal/dispatch/googleads.go` are main's provenance `defer`.

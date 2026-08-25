# 2026-08-24 — LFXV2-3067 the merge conflicted only in generated code

**Update** — merging `origin/main` brought LFXV2-3281's LinkedIn refresh attributes into
`design/connection.go`, which this branch also touches for the settings readback. The
design file auto-merged; the merged result was READ rather than trusted, because a
`design/` file in this repo has auto-merged into duplicate Goa methods before, and
duplicated methods compile and then behave wrongly. No duplicate `Method(...)` name and no
`…Endpoint2` is present.

All twelve conflicts were in DERIVED files — the four `gen/http/openapi*` specs, their four
`cmd/campaign-service/kodata/gen/http/` copies, and four generated CLI files. Choosing a
side in a generated file settles nothing, since the file is a function of the design; the
resolution is to clear the index with either side and re-derive everything from the merged
design via `make apigen`. Taking ours alone does not build, because the kept specs omit the
routes the merged design still declares.

**Verification** — the regenerated `openapi3.json` carries both sides: main's
`MDP-approved` refresh-token description and this branch's settings surface. Path-set
arithmetic shows a true union — ours 42, main 41, merged 42, nothing missing and nothing
invented.

The four kodata copies compare byte-identical to their `gen/http/` originals after
`apigen`, which is the pair the deployed pod serves and one CI does not check.

The design change was regenerated rather than hand-edited, so the spec cannot drift from
the DSL that produced it.

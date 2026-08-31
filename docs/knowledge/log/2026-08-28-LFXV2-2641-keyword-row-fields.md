# 2026-08-28 — LFXV2-2641 keyword and audience row fields

**Update** — The Google Ads keyword and audience reads now carry the ad group
and campaign
display names, a quality score, and conversions. All five are ordinary GAQL
fields that were
simply never selected, so the change is additive: no existing field changes
shape and no response
gets narrower.

Two of them have absence semantics that are not zero, and both are load-bearing.

`quality_score` is OPTIONAL and travels as a pointer through all four layers
(platform client →
dispatcher → domain model → Goa service). Google withholds the 1-10 rating until
a keyword has
accrued enough impressions, so an unrated keyword is the ordinary case rather
than an edge one,
and 0 is off the scale — any layer flattening nil to 0 would present every
unrated keyword as the
worst-rated one. Absence arrives at two levels (the `qualityInfo` block omitted,
and the score
omitted within a present block) and both collapse to nil.

A score outside 1-10 is dropped rather than published, and that guard protects
the WHOLE response
rather than one field: the design declares `Minimum(1)`/`Maximum(10)`, and Goa
emits response
validation in the generated client, so a single out-of-range row would reject
the entire keywords
response — the same failure mode the `match_type`/`status` `UNKNOWN` members
exist to prevent,
reached from the other direction. The row is kept; only the score is withheld,
because the row's
impressions and spend are real and unaffected. A test reads the bounds out of
`design/brief.go`
rather than restating them, so the guard cannot drift from the declaration that
makes it
necessary.

`conversions` is deliberately NOT a pointer: Google always measures conversions
for a served
keyword, so an omitted field is a measured zero. It keeps its fraction, and it
carries the same
magnitude validation `GetCampaignMetrics` already applied — NaN, ±Inf, a
negative count, or a
value beyond int64 fails the whole response. That guard was initially missed in
the shared metrics
helper, which mattered more on the audience path than the keyword one: a bad
value there is
SUMMED into a bucket, so one corrupt row poisons every figure in that bucket
rather than only its
own.

**Verification** — The five fields cross four layers and every hop maps them by
hand onto
value-typed struct fields, so a dropped field is invisible to the compiler and
silently returns
zeros. Deleting the mappings at the service layer and again at the dispatcher
layer left the build
completely clean while all five became zeros; only the assertions caught it.
Each hop is therefore
pinned to a distinct non-zero value. `go vet` — not `go build` — caught three
test fakes needing a
new port method.

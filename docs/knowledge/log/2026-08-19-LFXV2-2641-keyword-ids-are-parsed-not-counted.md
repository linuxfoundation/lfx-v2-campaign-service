# 2026-08-19 — LFXV2-2641 a keyword id is PARSED, not counted

**Fix** — `ValidateKeywordActions` bounded `ad_group_id` and `criterion_id` with
`len(id) > maxKeywordIDLen` where `maxKeywordIDLen = 20`. Google Ads ids are positive
`int64`s and `math.MaxInt64` (`9223372036854775807`) has NINETEEN digits, so a twenty-digit
id passed the cap, was digits-only, was injection-safe, and could not name a criterion that
exists. It reached the type-resolution GAQL request, where Google's PERMANENT rejection came
back through the read arm that classifies as a RETRYABLE 503 — an operator told to retry a
handle no number of retries can make valid.

Reproduced before changing anything: `ValidateKeywordActions` with criterion id
`"99999999999999999999"` returned `nil` error.

**A proxy for a constraint is not the constraint.** The real rule is "names a positive
`int64`"; a digit count only approximates it, and the gap is exactly where the invalid ids
live. Raising the cap to 19 would not have closed it either — `"9999999999999999999"` is
nineteen digits and still above `math.MaxInt64`, and neither `"0"` nor the leading-zero
spelling `"0305729261"` is long at all. Only parsing the value decides the question.

Both ids now go through `canonicalCampaignID`, which this package already uses to enforce the
same rule on campaign ids. It is REUSED rather than reimplemented so the two surfaces cannot
drift, and its round-trip through `ParseInt`/`FormatInt` collapses every spelling to one —
which is also what makes the `adGroupID+"~"+criterionID` keys in `resolveKeywordCriteria`
compare criteria rather than text. `"0305729261"` and `"305729261"` are the same criterion to
Google and two different map keys here, so admitting both spellings would let one batch
address one criterion twice past the duplicate check.

**The design was wrong too, and a runtime rejection without the matching design constraint is
half a fix.** `design/brief.go` declared `MaxLength(20)` on both attributes, so Goa's decoder
admitted the same impossible ids for HTTP callers. It is now `MaxLength(19)`, regenerated with
`make apigen` including the four `cmd/campaign-service/kodata/gen/http/openapi*` copies.
`maxKeywordIDLen` is 19 and now exists ONLY to keep that declared bound honest and in one
place — the client's check is the parse, not the count.

**On absence and defaults, since this is the second time the wire format has bitten.** The
sibling fix earlier today (`2026-08-19-LFXV2-2641-omitted-negative-is-positive.md`) failed the
same way from the opposite direction: Google Ads REST is protobuf JSON, and **protobuf JSON
does not serialise a field at its default value**. A `false` bool, a `0` number and an `""`
string are all sent by being ABSENT. The rule both fixes point at:

- **Absence in this wire format already MEANS the default.** It cannot be given a second
  meaning — "unknown", "unresolvable", "fail closed" — because the ordinary happy-path value
  is indistinguishable from it, and a re-read returns the identical omission.
- **A missing FIELD and a missing ROW are different facts.** A missing row is a real absence
  and may fail closed; a missing scalar field is a present value that was elided.
- **Fail-closed-on-absence is right for a ROW and wrong for a FIELD.** Applying the row rule
  to a field refuses the entire normal path.
- The mirror on the outbound side is already in the codebase: `campaignBudgetCreate.
  ExplicitlyShared` is a `*bool` specifically "so the false value is always emitted rather
  than omitted", and `campaign.go:255` reads an omitted `networkSettings` as no networks
  "— proto3 bools default false". When our own encoder needs a pointer to FORCE `false` onto
  the wire, that is the same fact seen from the sending end.

Corollary for validation generally, which is what this fix is: a constraint the wire format or
the domain actually imposes must be checked as that constraint. Bounding a stand-in for it
(digit count for `int64` range, presence for polarity) leaves a gap whose contents are exactly
the values the check was written to catch.

**Verification** — `TestValidateKeywordActions_RejectsIDsThatCannotNameACriterion` asserts
both sides: `math.MaxInt64` itself and an ordinary pair must be ACCEPTED, while twenty digits,
a nineteen-digit overflow, `"0"` and a leading-zero spelling must be REFUSED. The rejected
cases are chosen so a length-only check cannot pass the test — `"0"` and `"0305729261"` are
well inside any twenty-digit cap. The test also asserts `len(FormatInt(math.MaxInt64)) ==
maxKeywordIDLen`, so a future author restoring 20 is told the cap no longer matches the widest
id Google can issue rather than silently widening the surface again.

Two mutations, both compiling, both reverted:

- Restoring the `len(id) > maxKeywordIDLen` pair in place of the `canonicalCampaignID` calls
  fails four sub-tests — the nineteen-digit overflow, both `"0"` cases and the leading-zero
  spelling. The twenty-digit cases pass under this mutation ONLY because the constant is
  already 19, which is the point: the cap and the parse are two different guards.
- Restoring `maxKeywordIDLen = 20` fails the test at the constant assertion.

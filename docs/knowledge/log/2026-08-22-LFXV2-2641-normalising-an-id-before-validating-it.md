# 2026-08-22 — normalising an id before validating it moves the boundary

**Fix** — two sites on this branch called `strings.TrimSpace` on a caller-supplied identifier
and then validated the trimmed value. Both were reported by review, both were real, and they
fail in different directions.

## The tenant boundary

`campaignScopePredicate` builds the `campaign.id IN (...)` predicate that confines the keyword
and audience reads to one project's campaigns, on a Google Ads customer shared across every
foundation. It trimmed each id and then checked `customerIDRE` (`^[0-9]+$`). Digits-only bounds
the CHARACTER CLASS, which is all an injection guard needs, but this predicate is also an
IDENTITY check — and `"0"`, the leading-zero spelling `"000123"`, and a digit string past
`math.MaxInt64` all satisfy the class while naming no campaign.

`canonicalCampaignID` (campaign_lookup.go:128) already encodes the right rule for this package,
and its own doc states why it does not trim: `" 123 "` is not a value the API produces, and
trimming converts "this row is malformed" into "this row is campaign 123". That substitution
was being made inside the predicate that decides whose rows come back. The fix reuses
`canonicalCampaignID` on the RAW value, so the two surfaces cannot drift.

## The published contract

`ValidateKeywordActions` is the runtime backstop for the keyword-actions endpoint.
`design/brief.go` pins `Pattern(^[0-9]+$)` on `ad_group_id` and `criterion_id`, so Goa's
generated decoder rejects `" 333 "` for every HTTP caller before a handler runs. Trimming here
accepted that same value from a non-HTTP caller and silently rewrote it to `"333"` — the
transport layer and the backstop disagreeing about what a valid request is, with the more
permissive answer on the side that has no schema in front of it. The ids are now read raw.

The ACTION is still trimmed and upper-cased, deliberately: it is a closed enum this package
defines, so `" pause "` and `"PAUSE"` name the same member and no other campaign is reachable
by mis-normalising it. An id is an opaque upstream handle, and there is no such guarantee.

## What the tests had pinned

Two existing tests asserted the trimming as though it were the intended behaviour —
`TestCampaignScopePredicate_RendersAnUnquotedINList` passed `" 111 "` and expected `111`, and
`TestValidateKeywordActions_NormalisesAction` asserted padded ids came back trimmed. Neither was
testing padding: one asserts unquoted rendering, the other action normalisation. Both now use
canonical ids, and the padding cases moved to tests that assert REJECTION. A test can pin a
defect simply by having been written against it, and the tell here was that the property in the
test's name was not the property the fixture depended on.

## The live boundary test

`listProjectPlatformCampaignIDsQuery` had only SQL-SUBSTRING assertions. Those prove the clauses
appear in the text and nothing else — not that the two bind arguments land on the columns they
name, not that the exclusions remove rows, not that a nullable `result` scans.
`TestLiveListProjectPlatformCampaignIDsIsTenantScoped` seeds a same-provider row under a second
project, a second provider under the same project, a soft-deleted row, an empty id, a NULL id,
and one row of each provenance shape, then asserts the exact id SET. A count assertion would
have passed on the wrong two rows, which is the cross-tenant failure itself. Five mutations —
dropping each of the four clauses and swapping the bind arguments — each fail it.

The rule both halves share: validate the value the caller actually sent. Normalising first
means the check runs on a value the caller never supplied, and the gap between the two is where
one project's identifier becomes another's.

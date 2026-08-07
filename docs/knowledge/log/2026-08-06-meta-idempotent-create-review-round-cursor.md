# 2026-08-06 — Meta idempotent create: cancelled lookups keep their partial, and two test defects

**Update** — Cursor found that a caller context cancelled DURING the by-name campaign lookup
returned a bare `(nil, err)` worded "nothing created". That wording answers the wrong
question. Nothing was created *by this call* — but the lookup exists precisely to find a
campaign a PRIOR ambiguous attempt may already have created under the same deterministic
name, and a cancel leaves that unanswered. It is ambiguous in exactly the way a transport
failure is.

The consequence is concrete: a bare error makes `IsOutcomeUnconfirmed` false, so the
dispatcher records a clean failure and releases the claim, and the next retry POSTs the same
name again. Meta enforces no name uniqueness (unlike Google/Microsoft, which reject a
duplicate name and let the client self-heal), so that is a second PAID campaign — the exact
defect this lookup was added to prevent.

The cancel path now returns the name-carrying partial with a CUT SHORT step and joins
`errLookupAmbiguous` into the error so it classifies as UNCONFIRMED, with the cancellation
cause still reachable via `errors.Is`. The ad set lookup immediately below already returned
`partialResult()` on cancel for the same reason; the campaign lookup was the odd one out.

`TestCreateCampaignContextCancelDuringNameLookupRetainsPartial` pins it, and also asserts no
mutating call is made after the cut-short lookup. Verified binding: restoring the old
`return nil, ...` fails it with `expected a partial result carrying the campaign name`.

**Update** — Cursor also found `TestCreateCampaignReusesExistingByName` was not testing what
it claimed. Both by-name lookups are `filtering` GETs, and the fake answered every one with
the same campaign-shaped payload — so `findAdSetByName` "found" the campaign id as an ad set,
the ad set POST was skipped, and the reconciliation path the test exists to cover (reuse the
campaign, but still CREATE the ad set that does not exist under it) never ran.

The fake now discriminates on path — `/{account}/campaigns` vs `/{campaignID}/adsets` — and
returns an empty ad set page. The test asserts `AdSetID == "adset_1"` and exactly one ad set
POST alongside the zero campaign POSTs. Verified binding by restoring the conflated response:
it fails with `ad set id = "existing_camp_123"` and `ad set POST called 0 times`, which is
what the old fake was silently accepting.

**Update** — The `campaignPostCount` and `callOrder` counters in the new RoundTrippers were
written on the transport's goroutine and read on the test's without synchronization. The
client happens to issue these calls sequentially, but `-race` reasons about happens-before
edges rather than observed interleaving, and the surrounding tests already use the mutex
pattern. All four counters are now mutex-guarded, with the value copied out under the lock
before any assertion reads it.

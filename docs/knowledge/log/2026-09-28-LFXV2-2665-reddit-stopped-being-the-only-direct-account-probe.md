# 2026-09-28 — LFXV2-2665: Reddit stopped being the only direct account probe

**Fix** — round 28 of review, raised by the repo-code reviewer. No behaviour change; three
comments corrected and one doc table made symmetric.

Three sites said Reddit is "the one platform" whose probe addresses the configured account
directly instead of enumerating and checking membership, and called that "the strongest form of
the check" / "the strongest of the six". Both halves were true when written and neither is now:
this same change added X's probe, which reads its configured account root through
`twitter.Client.VerifyAccount` and is direct in exactly the same sense. `docs/api-catalog.md:241`
already records the pair — "Reddit and X read the configured account **directly**, which is the
stronger check" — and `TwitterDispatcher.ProbeConnection`'s own Godoc says outright that it is
"the same choice Reddit's probe makes, for the same reason".

So the contradiction was inside the change, between two files added by it. The corrected sites are
`reddit.Client.VerifyAccount`, `RedditDispatcher.ProbeConnection` and
`ConnectionService.TestRedditAds`; each now says two of the six probes are direct, names X as the
other, and distinguishes the requests — `GET /ad_accounts/{id}` against the account root. The
roster table in `internal-dispatch.md` had the facts right but graded only Reddit's row as "the
strongest form", so both direct rows now read "the stronger form".

Worth naming, because it is a different failure from the five stale-prose rounds before it. Those
were sentences left behind by a change to the thing they described. This one was never edited: it
was a true statement about a set, invalidated by adding a second member to the set somewhere else
in the same branch. Nothing about `reddit/probe.go` changed, and no sweep that starts from "what
did this commit touch" would look at it. A uniqueness claim is a claim about every sibling, and it
goes stale when a sibling is born.

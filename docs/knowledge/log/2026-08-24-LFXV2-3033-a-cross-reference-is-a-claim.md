# 2026-08-24 — a cross-reference is a claim, and it is package-scoped

**Fix** — the commit that corrected the flight-poison arm's reachability claim
(`c43d8100`) wrote one carefully-verified paragraph and then copied it verbatim into all three
provider clients. The paragraph ends `(see refreshToken)`. That name was true only in the
package it was written in.

| package | single-flight leader method | `refreshToken` defined? |
| --- | --- | --- |
| `internal/platform/reddit` | `refreshToken` | yes |
| `internal/platform/microsoft` | `accessTokenValue` | **no** |
| `internal/platform/googleads` | `accessTokenValue` | **no** |

So a commit written to make comments accurate introduced two inaccurate ones. The prose it
carried was correct — the leader really does set `inflight.token`, retract `c.inflight` and
close `done` in one unbroken critical section in all three — but the SYMBOL it named to
support that prose did not exist in two of the three packages, pointing the reader at nothing.

## Why the copy was not caught

The claim reads as a pure documentation change, and the reachability argument had been
measured with a racing probe, so review attention went to whether the ARGUMENT was true. It
was. The identifier riding along inside it was never separately checked, because a comment
edit does not compile, does not lint, and no gate in this repo resolves a name mentioned in
prose. `go vet`, `gofmt -s -l`, `golangci-lint` and the full `-race` suite were all green
across the introducing commit.

**A name inside a comment is an unchecked assertion.** Grep every symbol you name in the
package you name it in — a cross-reference has no compiler behind it.

## The same overstatement in the concept docs

The correction had also not been carried into `docs/knowledge/code/internal-platform-*.md`.
All three still asserted the arm was live: that a caller which missed the cache "joins the
still-published flight and is handed the very same token". Its premise is real — `fetchToken`
stores the token and unlocks, and the leader publishes under a LATER lock acquisition — but
the conclusion does not follow, because publication and retraction share one hold. The docs
now state the arm is defence-in-depth, not live coverage, and that it does not close the
pre-publication window tracked by #180.

**Correcting a claim in code is half the fix when the same claim was mirrored into the
knowledge bundle.** Grep the asserted phrase across `docs/` before calling the correction done.

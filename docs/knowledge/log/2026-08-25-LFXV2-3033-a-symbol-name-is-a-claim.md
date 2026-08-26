# 2026-08-25 — a symbol name in a comment is a claim, and this one was false

**Fix** — a comment on `cachedTwitterClient` said the build closure matched
"`cachedRedditClient`'s and `cachedMicrosoftClient`'s shape". `cachedMicrosoftClient` is real.
**`cachedRedditClient` does not exist and never did** — the only occurrence in the repository was
inside that comment.

Reddit's equivalent is `resolveRedditClientWithCreds` (`internal/dispatch/reddit.go:259`), and the
difference is not just spelling: it does the resolve and the cached build in ONE function and
returns `(client, *resolved, error)`, where Microsoft's and X's helpers take an already-resolved
credential and return a bare client. So the comment sent a reader hunting for a symbol that does
not exist, and the shape it asserted was not quite the shape either.

## Why it survived

The name is plausible. Three sibling helpers really are called `cachedGoogleAdsClient`,
`cachedMicrosoftClient` and `cachedTwitterClient`, so `cachedRedditClient` pattern-matches
perfectly against a convention that Reddit happens not to follow. Nothing mechanical catches it:
the compiler does not read comments, `go vet` does not, and neither does any linter in the gate.

## The check that would have caught it, applied to everything

After the finding, every symbol named in a comment this branch added was extracted and resolved
mechanically — 38 distinct names across the Go and Markdown diff, including 12 test-function
names, the constants (`writeDelay`, `maxRetryWait`, `DefaultBaseURL`), the fields (`writeMu`,
`nextWrite`, `onAdmit`) and the functions (`buildOnce`, `cacheIdentity`, `doRequestAbs`,
`isWriteMethod`, `nextWriteAt`, `sleepCtx`). **Exactly one did not resolve**, and it was the one
the reviewer had already named.

That ratio is the point. The sweep cost one command and confirmed 37 names were fine; had it been
run before the comment shipped it would have cost the same and caught the 38th.

## The rule

A symbol name in prose is an assertion about code, and it ages like any other assertion — except
that nothing in the build will ever tell you it went stale. `grep` it when you write it, and
`grep` it again when you edit the line around it. When a name is inferred from a convention
("the others are all `cachedXClient`, so Reddit's must be too"), that is exactly when it needs
checking, because a convention with one exception reads identically to one with none.

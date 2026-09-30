# 2026-09-29 — LFXV2-2665: four second-round review fixes to X tweet authoring

**Fix** — the rerun of the local pre-PR review over the same
`ceca731b..559fa41c` range returned four defects in the code the FIRST round's
fixes introduced, plus one finding about commit history. Each of the four is a
case where the first fix was right about the problem and too narrow about the
surface, so each is fixed at the surface rather than at the reported instance.

**1. Credential screening saw the registration URL, not the tweet.** Round one
screened `RegistrationURL`, but what gets published is the COMPOSED text, and
`TweetText` is carried into it verbatim — a caller's own pasted link went to X
unscreened. `rejectCredentialQueryParamsInText` now runs over every URL in the
composed text, after `composeTweetText` and before anything mutates, so the
artifact that is checked is byte-for-byte the artifact that is published and a
later change to how text is composed cannot route a URL around the gate.

**2. The key check was an exact-name set, and credential names COMPOSE.**
`secret_token`, `access_key`, `auth_key`, `oauth_token_secret` are credentials
under any reading and all normalised to entries the set did not have; the set
was the wrong SHAPE, not merely short, because enumeration does not converge.
`isCredentialQueryKey` is now a three-tier predicate over the normalised key:
the exact set for spellings that carry no fragment; a fragment list of words
unambiguous as a COMPONENT of a compound (`token`, `secret`, `signature`,
`hmac`, `bearer`, `oauth`, `authorization`, `assertion`, …); and a "…key"
SUFFIX rule minus an explicit benign set. The tiers are the design, not an
accident: `key`, `auth`, `sig`, `pass` and `session` are exactly the words that
cannot be fragments — `keyword`, `design`, `bypass`, `passenger` — so they stay
exact-only, and `key` gets the suffix rule instead, the one position where it
really is one. `code` and `pin` remain excluded.

**3. `tweetURLRe` was case-sensitive and ran into sentence punctuation.**
RFC 3986 §3.1 makes the scheme case-insensitive, and X wraps `HTTPS://…` to
t.co like any other link, so the case-sensitive match charged it raw length and
invented a rejection of valid copy. `\S+` also swallowed a trailing period, which
both under-counts the weight and — now that the same run is parsed for fix 1 —
hands `url.Parse` something other than the URL that will be fetched. The pattern
is `(?i)\bhttps?://\S+` and each run passes through `trimTweetURLPunct`, which
peels trailing `.,;:!?'"` and unmatched closing brackets. The trimmed link is a
PREFIX of the run, so it still locates at the run's offset in `weightedTweetLen`;
advancing by its length alone leaves the punctuation to be weighted as prose.

This is the deliberate OPPOSITE of `sanitizeSnapshotText`
(`internal/dispatch/creds.go`), which takes the greedy run and must not trim, and
the concept file says so where a future reader would otherwise unify them: there,
over-reach fails SAFE and trimming could leave credential text outside the run;
here over-reach fails UNSAFE in both directions.

**4. `maxListPages` exhaustion was treated as the end of the list.**
`resolvePromotableUser` fell out of its loop and every conclusion below it is a
claim about the WHOLE list — "not among them", "none at all", and the
single-candidate auto-pick, which on a truncated list picks an author from a set
the caller never saw. It now tracks whether the walk actually terminated and
refuses as inconclusive otherwise, which is what `findByName` already did.

**5. Page cursors reached rendered, persisted errors.** A cursor is opaque text
decoded from an upstream response body, and `doRequestAbs` writes its `logPath`
into every `apiError`, `transportError` and `preSendError` — which on this path
become `PromotedTweetWarning` and a `Steps` entry. Folding the cursor into the
path therefore wrote upstream response text into the campaign record, and the
repeated-cursor error named the cursor outright. That is
`platform-error-must-not-carry-untrusted-or-credential-text`. The new
`requestPage` keeps the cursor on the wire URL and passes the query-free path as
the log path, and the repeated-cursor refusal is now static, as
`ListAdAccounts` already was. It is applied to `findByName` as well as
`resolvePromotableUser`: both account-scoped walks had the identical leak, and
`requestPage` exists because the two had no shared way to follow the precedent.

**On the history finding.** The review also asked for the range to be squashed
because intermediate commit `95a4471a` "emitted promotable-user IDs". `git show`
of that commit contains format strings only — `%v` of `ids`, `%q` of `pinned` —
so no identifier exists in the repository to remove. The range also contains a
merge commit whose recorded conflict resolution a squash would discard, and every
PR in this repo lands on `main` as a single squashed commit, so the intermediate
never reaches `main` regardless. Not rewritten; the PR states the squash-merge
requirement explicitly instead of relying on it.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the
cap-exhaustion refusal and the cursor-hygiene rationale for `requestPage`, the
URL-run boundary rules and why they diverge from `sanitizeSnapshotText`, and the
credential predicate replacing the exact-name denylist). `docs/api-catalog.md`'s
X config table now says the screen covers every URL in the composed text and
recognises compounds, that URL weighting is case-insensitive and excludes
trailing punctuation, and that an exhausted page cap is refused rather than
concluded from.

Refs: LFXV2-2665

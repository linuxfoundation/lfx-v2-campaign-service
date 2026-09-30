# 2026-09-30 — LFXV2-2665: a URL scanner is only as good as its boundaries

**Fix** — the eleventh `/lfx-skills:lfx-local-review` cycle on the X authoring work
returned four findings against `4169fea1`. Three are fixed, one declined. Three of the
four findings are the same defect seen from three sides: the two scanners that decide
what a URL *is* were both wrong at their edges, and the credential denylist overpromised
in the catalog.

## 1. `_https://…` bypassed the publication screen AND the snapshot redactor

Both `tweetURLRe` and `snapshotURLRunRe` opened with `\b`. Go defines `\b` over `\w`, and
`\w` includes `_`, so there is no word boundary between the underscore and the `h` of
`_https://…` and neither expression matched the run at all.

Measured, before the fix: `_https://events.example/cb?access_token=PLAINTEXT` was
published verbatim in the authored tweet *and* left intact in the UNENCRYPTED
`config_snapshot`. An underscore before a link is not a contrived input — it is how
markdown italicises one and how a link arrives out of most chat clients an operator
pastes from.

RE2 has no lookbehind, so the boundary cannot live in either pattern. The two scanners got
different answers on purpose:

- **`internal/platform/twitter`** keeps a boundary rule, applied in code by
  `findTweetURLRuns`: a scheme is at a boundary unless a letter or digit precedes it. It
  keeps one because this scanner also drives `weightedTweetLen` and `textCarriesURL`,
  where over-matching changes an answer — a run that is not a link still gets charged the
  fixed t.co weight, and a destination that "matches" wrongly is one the client stops
  appending. Every consumer now goes through `findTweetURLRuns`, so the screen, the
  weighting and the destination match cannot drift on what a link is.
- **`internal/dispatch`** drops the boundary entirely. Repairing `_` alone would have left
  the identical hole one character over, and this function redacts what it finds and
  leaves everything else untouched, so matching a scheme buried inside a longer word costs
  a mangled fragment of prose in a snapshot while missing one costs a persisted token.
  That is the same asymmetry the stop set in that file was already chosen for.

## 2. Scheme-less links were never screened, and X publishes them

`tweetURLRe` requires `http://` or `https://`. X linkifies `www.events.example/r?…` and
bare `events.example/r?…` exactly as it linkifies a scheme-ful link, and publishes them to
the promoted-tweet audience the same way — so an operator who pasted a scheme-less link
out of a logged-in browser had no guard at all.

`findSchemelessScreenRuns` covers that shape and feeds the **screen only**. The reviewer
asked for twitter-text-compatible extraction shared with `weightedTweetLen`; that half is
declined below. Screening more runs than X links costs a refusal the operator fixes by
deleting a parameter. Weighting more runs than X links costs a working brief, rejected
before the create, at the 280 boundary.

What keeps it out of ordinary copy is the `?`/`#` requirement. This screen only ever asks
whether a query or fragment parameter names a credential, so a run with neither has nothing
for it to read — which drops `agenda.md`, `Node.js`, `v1.2` and every other dotted token in
real prose out of the candidate set before any parsing happens. An alphabetic, two-character
minimum on the TLD keeps `3.2?` out. Scheme-ful runs are blanked by byte offset first, so
no link is screened twice.

## 3. `api_key_LEAK` cleared the screen entirely

The `…key` tier only fires on a name that ENDS in `key`. A key name with the credential's
own value appended — `api_key_LEAK`, `access_key_AKIAIOSFODNN7`, which is how an exported
link most often spells one — ends in the value, matched no tier, and was published.

**Round 10 found this and left it open**, on the reasoning that closing it meant promoting
`key` to a plain component match and thereby refusing `key_metrics` and `key_takeaways`:
real parameters on a real conference page. The finding is right that this was a hole and
the reasoning was the wrong shape for the fix, not a reason to keep it. `key` on its own is
ambiguous; `api key`, `access key`, `secret key` are not, on any page, in any spelling. The
new tier is about the PAIR — a `key` component with a qualifier from a declared set
standing directly in front of it — so it needs no new judgement about ordinary English.
`key_metrics` has no qualifier before `key` and still passes; `sort_key` is still caught by
the suffix rule. The rendered term is two declared literals joined, so the refusal still
names our vocabulary and never the caller's bytes.

The catalog wording that prompted the finding is rewritten alongside it. It claimed keys
were refused "under any reading", "compounds included", which was an overpromise even
before this gap: it now enumerates the four tiers as a denylist and states plainly what it
does **not** catch — a credential parameter sharing no word with the lists, and a secret in
the URL's PATH.

## Declined, with reasons

- **Strip the path from the persisted "Destination URL template" step.** Third time, and
  the trigger genuinely did broaden — the step now fires on every `tweetText` authoring
  failure, not only the historical manual workflow — but that changes how often the step
  is written, not what it is. It is a TEMPLATE the operator pastes into a manually composed
  tweet, and the finding's own remedy ("direct the operator to retrieve the full
  destination from the brief") deletes the only reason the step exists: it was added
  precisely because gating it left the `tweetText` caller with strictly less than the
  caller who supplied nothing, rebuilding the utm_* set by hand. `https://events.example`
  is not a destination. The path here is also not a secret leaking into `Steps` — it is the
  ad's click destination, the one part of this flow the operator is deliberately asking to
  publish, and an ad whose destination is a single-use magic link is already broken as an
  ad. Same conclusion as round 10's path-screening decline, and the same standing offer:
  worth revisiting with evidence of a real brief carrying a secret-bearing path, not worth
  guessing at.
- **Share twitter-text URL extraction between screening and `weightedTweetLen`.** The
  screening half is fixed above. The weighting half needs twitter-text's TLD registry to
  decide which dotted tokens X actually linkifies, and every token it gets wrong near 280
  is a create refused for copy X would have accepted. The two scanners are documented as
  deliberately different, with the reason attached.

## Coverage

Each fix was verified by reverting it alone from a scratchpad copy:

- Restoring `\b` to `snapshotURLRunRe` fails `TestSanitizeSnapshotText_UnderscorePrefixedURL`
  on all four rows, printing the surviving credential:
  `sanitizeSnapshotText("_https://events.example/cb?access_token=SECRET")` returns its own
  input.
- Removing the scheme-less loop fails `TestRejectCredentialQueryParamsInText_SchemelessLink`
  on all four refusal rows.
- Removing the qualified-`key` tier fails `TestCredentialQueryKeyMatch_QualifiedKeyComponent`
  on `api_key_LEAK`, `access_key_AKIA…`, `API-KEY-LEAK`, `client.key.LEAK`, `signing_key_v2`.
- Restoring `\b` to `tweetURLRe` fails `TestTweetURLRuns_UnderscoreIsADelimiter` on both
  halves — `weightedTweetLen` charges 143 instead of 24, and `textCarriesURL` misses the
  destination, so the client appends a second copy of it.

That last revert is worth recording precisely, because it is the one place the evidence is
partial. `TestRejectCredentialQueryParamsInText_UnderscorePrefixedURL` still PASSES with
`tweetURLRe`'s `\b` restored — the scheme-less screen from fix 2 catches the same text by a
different route. The twitter-side boundary fix is therefore load-bearing for weighting and
destination matching, which the test above pins, and redundant for screening. Both fixes
stay: the screen should not depend on a scanner that was not written for it.

A limit left in place deliberately: a trailing `_` is swept into a URL run, because
`tweetURLTrailingPunct` does not list it, so the closing half of markdown italics still
defeats `textCarriesURL` and the client appends a duplicate destination. Adding `_` to the
trim set moves the same cosmetic failure onto destination URLs that legitimately end in one.
Recorded rather than traded sideways.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the two scanners
and why they differ; the qualified-`key` tier) and `docs/knowledge/code/internal-dispatch.md`
(why the snapshot redactor has no boundary at all). `docs/api-catalog.md` rewritten for the
exact denylist behaviour and its stated limits.

Refs: LFXV2-2665

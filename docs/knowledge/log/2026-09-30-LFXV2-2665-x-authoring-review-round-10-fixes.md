# 2026-09-30 — LFXV2-2665: a parameter NAME can hold a secret, and two rounds of declining that was wrong

**Fix** — the tenth `/lfx-skills:lfx-local-review` cycle on the X authoring work
returned four findings against `a6079ccc`. Two are fixed, two declined.

## 1. The refusal echoed the caller's query key, and a key is free text

`rejectCredentialQueryParams` named the offending parameter by reproducing the
caller's key through `safeQueryKeyForError`, which strips control characters and
truncates to 40 runes. The justification, written into the code and this bundle, was
that a parameter NAME cannot be the secret because the secret is the VALUE, which the
parser holds separately and never renders.

That is wrong, and **this review raised it twice before and was declined both
times.** `?oauth_token_<secret>=x` classifies on `oauth` and then reproduces
`oauth_token_<secret>` in an error that reaches the dispatcher, the campaign's
persisted `Steps` and the service log. Bounding is not redacting; a credential prefix
is still credential material. The knowledge base already states the test this fails —
reproduce a component only when it is BOTH structurally incapable of holding a secret
AND load-bearing — and a caller-written name fails the first half outright. The
earlier declines leaned on `named[key]` proving the key is a parameter NAME, which is
true and beside the point: being a name says nothing about what the name CONTAINS.

`credentialQueryKeyMatch` now returns the matched term alongside the verdict, and the
term is always a literal from this package's own lists — an exact-set entry, a listed
substring, a listed component, or `key` for the suffix tier. The error names OUR word,
never THEIR key. That is the same default-deny shape as `safeCause` in
`internal/platform/hubspot/client.go`, and it keeps the load-bearing half: the
operator finds the parameter by searching their own URL for that word, which is
exactly how they would have used the key itself.

`safeQueryKeyForError` is deleted rather than left unused — nothing caller-controlled
reaches an error message on this path any more, so there is no untrusted text left for
it to bound.

## 2. `_csrf`, `xsrf` and `SAMLResponse` cleared the publication screen

All four classifier tiers returned false for them. `csrf_token` and `x_csrf_token`
were already caught by the `token` fragment, which is what hid this: the bare `_csrf`
that Rails, Spring Security and Express all emit normalizes to `csrf` — no exact
entry, no listed fragment, not a component, no `key` suffix — and `SAMLResponse`
carries a signed assertion in a single parameter matching nothing at all. Their values
were copied verbatim into the published tweet.

Added to the substring tier, where they are safe as fragments: no ordinary English
word and no routing parameter an events page uses contains `csrf`, `xsrf` or `saml`.
The test pins both halves — the credentials are refused, and `session_track`,
`day_pass`, `keyword`, `author`, `aside` and `design` still pass.

## Declined, with reasons

- **Reject credential-bearing URL PATHS (`/reset/<token>`, `/magic/<token>`) before
  publishing.** A new finding, and a real sink — but every available screen is worse
  than the exposure. Running the existing classifier over path segments refuses
  `/sessions/`, a real path on real event sites, because `session` is an exact-set
  entry; that is the working-brief regression the denylist shape exists to avoid. The
  reviewer's alternative — an allowlist of accepted public URL patterns — cannot be
  written for a field whose entire purpose is to carry whatever URL the brief names.
  And a route-word denylist (`reset`, `magic`, `verify`) is new invented vocabulary
  with no evidence behind it. The destination is also the one part of this flow the
  operator deliberately asks to publish, and an ad whose destination is a single-use
  magic link is already broken as an ad. Worth revisiting with evidence; not worth
  guessing at.
- **Mask the host (`https://xxxxx`).** Sixth time. The KB's verbatim quote is about
  `redactAIProxyURL`, where the INTERNAL proxy hostname was itself the deployment
  secret. A public event-registration host is not, and the host is the load-bearing
  half of what a redacted URL can still tell its reader.

## Coverage

Both fixes were verified by reverting them alone from a scratchpad copy:

- Reverting the substring additions fails `TestIsCredentialQueryKey_CatchesCSRFAndSAML`
  on `_csrf`, `csrf`, `xsrf`, `SAMLResponse` and `SAMLRequest`.
- Reverting to `%q` on the caller's key fails
  `TestCreateCampaign_RejectsCredentialQueryParamBeforeAnythingIsCreated` on every
  row, through the shared `assertNamesClassifyingTerm` helper — which checks both that
  the classifying word is present AND that the rendered term is a declared literal, so
  it cannot be satisfied by echoing caller text.

`TestCredentialQueryKeyMatch_NeverReturnsCallerText` covers the structural property
directly, over keys with secrets appended to credential names.

A limit found while writing that test and left in place deliberately:
`access_key_AKIA…` normalizes to a name that no longer ENDS in `key`, so the suffix
tier misses it, and `key` is not a component match because `key_metrics`-shaped
routing parameters exist. Widening either would contradict a decision weighed where
the tiers are declared and would refuse working briefs. The gap is recorded here
rather than closed by guess, and the test is scoped to what the refusal RENDERS, which
holds regardless.

Concept file updated: `docs/knowledge/code/internal-platform-twitter.md` (the term-not-key
rendering, and why the two earlier declines were wrong). `docs/api-catalog.md` updated
for the new vocabulary entries and the changed error contract.

Refs: LFXV2-2665

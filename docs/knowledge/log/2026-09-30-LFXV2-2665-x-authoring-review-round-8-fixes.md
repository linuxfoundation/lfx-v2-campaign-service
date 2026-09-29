# 2026-09-30 — LFXV2-2665: the round-7 snapshot fix failed open, and the destination fragment was never routing-neutral

**Fix** — the eighth `/lfx-skills:lfx-local-review` cycle on the X authoring work
returned four findings against `dbeb684e`. Two are fixed, two declined.

## 1. `sanitizeSnapshotURL` failed open on an http-shaped value it could not reduce

All three reviewers converged on this, and they were right: the round-7 path-drop
only applied on the success branch. Anything that reached the fallback — a value
that does not parse, or parses with an empty host — was truncated at `?`/`#`,
checked for `@`, and otherwise **returned whole, path included**. That is the exact
exposure the reduction was added to close, reachable through two ordinary shapes:

- `https:///reset/SECRET` parses cleanly, `IsAbs()` is true, `Host` is empty, and
  there is no `?`, `#` or `@` anywhere to truncate at — so it came back unchanged.
- `https://example.org/reset/SEC%zz` fails to parse on the bad escape and likewise
  came back unchanged.

An http(s)-scheme value that will not reduce to scheme+host now returns `""`. The
truncating branch stays for a value that never claimed to be a URL — a reddit thing
id, say — where it remains the conservative answer.

Nothing legitimate is lost, because every caller supplies a URL: reddit `PostURL`
and `ImageURL`, meta `ImageURL`, and the runs `sanitizeSnapshotText` feeds in, which
`snapshotURLRunRe` only ever matches starting from an http/https scheme. A value
that announced an http scheme and will not reduce is malformed input, not data with
a different meaning.

The scheme test is case-insensitive (`isHTTPScheme`), because the run regex is: a
case-sensitive test would have let exactly the `HTTPS://` runs fall through to the
branch the check exists to keep them out of.

## 2. The destination URL's fragment was dropped, and it is not routing-neutral

`buildTwitterUTMURL` cleared `Fragment`/`RawFragment` from the ad's REAL click
destination, documented as safe because a fragment is never transmitted to a server.
That is true of the server and false of the page:

- `#register` scrolls to and focuses the registration form — the whole reason a brief
  writes that URL rather than the bare page.
- A hash-router SPA reads the fragment as the ROUTE. `https://events.example/#/register`
  with its fragment removed is not the same page without an anchor; it is the front
  page.

So paid clicks landed somewhere the brief did not ask for, and silently: the create
succeeded, and `displayTwitterUtmURL` — which legitimately strips the fragment for
persistence — made every step we print show a destination that looked correct.

The fragment is now published verbatim alongside the query, for the same reason the
query is. The exposure argument does not survive the comparison: the fragment is one
component of a URL this function already publishes whole.

**The strip was also hiding the fragment from its own screen.**
`credentialFragmentError` was added in round 6 specifically to refuse a
credential-shaped fragment on a published URL — the OAuth implicit-flow shape,
`#access_token=…` with no query at all. But the screen runs over the COMPOSED tweet
text, and the build stripped the fragment before composition, so that arm never once
examined the registration URL it was written for. It only ever saw links the operator
typed into their own copy. Publishing the fragment puts it back under the screen,
which is what protects it; the strip only made it invisible. A plain section anchor
still passes, having no `key=value` pair — and now actually reaches the browser.

## Declined, with reasons

- **Strip the path from `displayTwitterUtmURL` too, so `Steps` cannot carry it.** The
  knowledge-base rule is a two-part test, and the path passes its load-bearing half
  here in a way it does not in `config_snapshot`. This step is the destination
  TEMPLATE an operator pastes into X's composer when authoring degrades; a
  scheme-and-host-only "destination template" is not a destination. The reviewer's own
  suggested remedy — tell the operator to reapply the path from the brief — concedes
  the value stops functioning. The same reasoning accepted the strip for the snapshot,
  where there is no in-the-moment consumer at all, and that asymmetry is the test
  working rather than an inconsistency.
- **Mask the host as well (`https://xxxxx`).** Fourth time for this family. The KB's
  verbatim quote is about `redactAIProxyURL`, where the INTERNAL proxy hostname WAS
  the deployment secret. A public event-registration host is not, and the contrived
  counter-example (`https://sup3r-s3cret/`) is a single-label hostname that resolves
  nowhere and that no brief contains. The host is the load-bearing half — "which site
  did this link point at" is the whole of what a redacted URL can still tell its
  reader. Fix 1 above narrows the reviewer's actual concern independently: the
  fail-open fallback it cited in the same finding is now closed.

## Coverage

Both fixes were verified by reverting them alone and re-running:

- Removing the `isHTTPScheme` guard returns all three new `TestSanitizeSnapshotURL`
  rows (`https:///reset/SECRET`, `https://example.org/reset/SEC%zzRET`, and the
  uppercase-scheme row) unchanged, path and all.
- Restoring the fragment strip fails `TestBuildTwitterUTMURL_KeepsQueryAndFragment`
  (fragment gone, query/fragment ordering assertion), the new
  `TestBuildTwitterUTMURL_KeepsAHashRouterRoute` (route lost, destination collapses to
  `https://events.lf.org/?utm_…`), and the new
  `TestCreateCampaign_RefusesACredentialFragmentOnTheRegistrationURL` — which fails in
  the way that proves the second half of the finding: the credential fragment reaches
  NO screen at all and the create runs on past it.

`TestBuildTwitterUTMURL_KeepsQueryDropsFragment` was renamed and rewritten to the new
contract rather than deleted, keeping its query-preservation and display-form
assertions.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md`,
`docs/knowledge/code/internal-dispatch.md`. `docs/api-catalog.md` updated for the
published fragment and the snapshot's fail-closed reduction.

Refs: LFXV2-2665

# 2026-09-30 — LFXV2-2665: six fixes from the seventh local pre-PR review of X tweet authoring

**Fix** — the seventh `/lfx-skills:lfx-local-review` cycle on the X authoring work
returned six findings against `ad13d8f1`. Five are fixed here, one of them split into
two changes; the reasoning for the three declines is recorded below so it is not
re-litigated in an eighth round.

## 1. `safeTransportCause` is now an allowlist — reversing round 6's decline

Round 6 declined this on the grounds that after every `*url.Error` layer is peeled,
what remains is a `net.OpError`, a syscall errno or `io.EOF`, none of which carries a
URL. That reasoning assumed the stdlib transport. This repo's own
`internal/platform/hubspot/client.go` already documents why it does not hold:
`WithHTTPClient` accepts a caller-supplied `RoundTripper`, so the innermost cause is
CALLER-CONTROLLED text and peeling "is NOT sufficient on its own". `PromotedTweetWarning`
and `Steps` are persisted, so a transport that returns `errors.New("upstream said: " +
signedURL)` wrote the signed URL into the campaign row.

`safeTransportCause` is now a fixed vocabulary with a default-deny of
`transport failure`, mirroring `hubspot.safeCause` and the microsoft equivalent so the
three clients fail the same way under the same threat. Each named case emits THIS
package's own string rather than the error's: a custom transport's timeout error is
still caller-controlled text even where the timeout classification is trustworthy. The
real cause stays reachable through `Unwrap()`.

The decline was wrong on the facts, not on a judgement call, which is why it was
reversed rather than restated.

## 2. Renderability was tracked per key, not per occurrence

`queryKeysWrittenWithAValue` answered "was this key ever written `name=value`". One `=`
anywhere was enough, so `?oauth_token_SECRET&oauth_token_SECRET=x` decoded to a single
key whose VALUED occurrence marked it renderable — and the error then reproduced a
string whose BARE occurrence is the whole credential. A key is now renderable only when
EVERY occurrence carried a value.

A duplicated key is a strange thing for a brief to carry, and that is the point: the one
shape that defeats the check is the one nobody writes by accident. Requiring every
occurrence to be named costs nothing on ordinary input, where a key appears once.

## 3. The fragment screen rendered its key unconditionally

`credentialFragmentError` was added in round 6 and did not inherit the name-vs-category
split the query path had had since round 4 — a defect introduced by the round-6 fix, not
one it inherited. `#access_token_<token>&state=…`, where the credential IS the component
text, was reproduced verbatim in the refusal. The fragment now applies the same split.

A fragment with no `=` at all is still not screened, and deliberately: that is the
section-anchor form (`#speakers`, `#agenda-day-2`), and refusing it would fail a working
brief for nothing.

## 4. "Verbatim" was not verbatim

`buildTwitterUTMURL` parsed the registration URL's query and re-emitted it with
`url.Values.Encode`, which SORTS keys and re-canonicalizes escaping — `%20` becomes `+`.
Both this file's concept doc and `docs/api-catalog.md` promise the pre-existing query is
preserved verbatim, on the ad's REAL click destination, and the step that claimed to keep
that promise was the step that broke it.

`url.ParseQuery` is still called, but now only to VALIDATE — the fail-closed behaviour
from round 5 is unchanged. `appendUTMToRawQuery` then copies each pre-existing component
as BYTES and appends the UTM pairs in sorted key order. Only a component whose decoded
name collides with a UTM key is dropped, because a destination carrying two `utm_source`
values makes click attribution depend on which one the landing page reads first.

## 5. A nested destination made the append silently skip

`composeTweetText` decided "the text already has the destination" with
`strings.Contains`. A URL is a substring of any URL that carries it in a redirect or
tracking parameter, so copy holding `https://click.example.net/r?next=<dest>` satisfied
that test, the append was skipped, and X wrapped the whole run as the OTHER link. The ad
then had no direct click destination at all — the one thing the append exists to
guarantee — and the create SUCCEEDED, so nothing surfaced it.

The new `textCarriesURL` extracts URL runs with the same `tweetURLRe` +
`trimTweetURLPunct` pair `weightedTweetLen` counts with and requires a whole-run match,
so the two places in this file that decide "where does a link end" cannot drift. Runs are
trimmed before comparison, so a destination ending a sentence still counts.

## 6. `sanitizeSnapshotURL` kept the path

`config_snapshot` is persisted UNENCRYPTED and outlives the campaign, and the sanitizer
kept scheme+host+path. `caller-url-must-be-redacted-before-errors-steps-and-snapshots`
addresses this directly: `https://litellm.example.com/sup3r-s3cret/v1` parses with the
token as a PATH segment, and `redactAIProxyURL` took four rounds to stop assuming that
whatever `url.Parse` had split out was structurally safe. A password-reset or magic-link
URL is the realistic shape here and survives a query-and-fragment strip untouched.

The HOST stays, and that is the same two-part test reaching a different answer: this
column's only reader is a human reconstructing what a campaign was configured with, and
"which site did this link point at" is the whole of what a redacted URL can still tell
them. The path is not load-bearing for that. Over-redacting costs nothing here, because
unlike an operator-facing error this value is never used to diagnose anything in the
moment — which is exactly why the same finding was DECLINED for `redactURLForError`
(below) and accepted here.

Scheme+host is rebuilt through `url.URL.String()` rather than concatenated: `URL.Host`
holds the DECODED authority, so a zone-scoped IPv6 literal would come back as
`[fe80::1%eth0]` — a bare `%` that is not a valid escape — turning a well-formed URL into
one that no longer parses.

## Declined, with reasons

- **Move `text`/`as_user_id` out of the query string.** Unchanged from rounds 4 and 6:
  the transport change is larger than the exposure, which is already closed at every
  rendering site. Worth a ticket rather than this branch.
- **An allowlist of non-PII routing parameters over the published destination.** Third
  time. An allowlist over an LF event page's routing and attribution parameters refuses
  working briefs to protect against nothing; the denylist shape was chosen for that
  reason and the reasoning has not changed.
- **Drop the HOST from `redactURLForError` too.** The knowledge-base rule is a two-part
  test, and a host passes both halves where a path fails one: a host is structurally
  incapable of holding a secret, and it is the only thing that tells an operator WHICH
  link in their copy is the offending one. `redactAIProxyURL` is not the precedent it
  looks like — there the internal proxy hostname WAS the secret.

## Coverage

Every fix has a test verified to FAIL against the pre-fix code, by reverting that fix
alone and re-running:
`TestSafeTransportCause_DoesNotRenderACustomTransportsText` (unit vocabulary plus an
injected `RoundTripper` asserting the warning and every `Steps` entry),
`TestRejectCredentialQueryParams_BareAndValuedOccurrencesOfTheSameKey`,
`TestCredentialFragmentError_NamesAValuedKeyAndRedactsABareOne`,
`TestBuildTwitterUTMURL_PreservesTheRawQueryBytes`,
`TestComposeTweetText_AppendsWhenTheDestinationIsOnlyNestedInAnotherURL`, and
`TestSanitizeSnapshotURL` / `TestSanitizeSnapshotText` with the path cases added.

Four existing tests asserted the OLD snapshot contract (the path surviving) and were
updated to assert the new one, each keeping its secret-absence assertion and gaining a
path-absence one. `TestTransportError_DoesNotLeakURL` now asserts the allowlist's
classification rather than the cause's own text.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md`,
`docs/knowledge/code/internal-dispatch.md`. `docs/api-catalog.md` updated for the
byte-preserved destination query and the scheme+host snapshot rule.

Refs: LFXV2-2665

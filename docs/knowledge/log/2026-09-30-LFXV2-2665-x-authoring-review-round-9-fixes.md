# 2026-09-30 — LFXV2-2665: the round-8 fragment fix opened a publish path, and "byte for byte" still reassembled

**Fix** — the ninth `/lfx-skills:lfx-local-review` cycle on the X authoring work
returned eight findings against `62b10fea`. Four are fixed, four declined.

## 1. A bare fragment reached the published destination unscreened

`credentialFragmentError` passed every fragment with no `=` in it, unconditionally.
Round 7 reasoned that a bare fragment has no key to test and is how a brief links
into a registration page, which was right — and safe, because `buildTwitterUTMURL`
stripped the fragment before publication. **Round 8's own fix removed that strip**,
turning the exemption into a publish path: `#access_token` standing alone would have
gone out verbatim in the authored tweet.

A bare fragment now runs through `isCredentialQueryKey` under the same
name-vs-category split the query path uses. With no `=`, the whole fragment landed
in the KEY position, so its text may BE the credential — the refusal names the
category and never echoes the value.

The anchors this had to keep working were measured, not assumed: `register`,
`agenda-day-2`, `speakers`, `sessions`, `session-track`, `schedule`, `sponsors`,
`venue`, `keynote` and `day-pass` all clear the classifier. Bare `#session` is the
one realistic anchor refused, and it is refused by an exact-set entry that earns its
place on the query side. The limit is stated rather than papered over: this catches
credential-NAMED shapes, not every credential — a raw `#eyJhbGciOi…` still passes.

## 2. `snapshotURLRunRe` stopped at an apostrophe, which is legal in a query

The run regex's stop set is meant to hold characters RFC 3986 excludes from a URI
outright. `'` was in it and does not belong: an apostrophe is a sub-delimiter, legal
in a query. So `https://events.example/reg?x=discard'api_token=SECRET` matched only
as far as the quote, and `'api_token=SECRET` stayed in `config_snapshot` as bare
prose — the same leak the `]` fix closed in an earlier round, through a character an
operator can type by accident.

`'` is out of both character classes. The DOUBLE quote stays, because `"` cannot
appear in a URI unescaped. Over-matching is the safe direction: swallowing an English
possessive's `'s` costs a snapshot nothing, and stopping one character early costs it
a secret.

## 3. `appendUTMToRawQuery` reassembled a query that had no collision in it

The byte-fidelity promise was still not literally true. Splitting on `&` and
rejoining normalises a query's empty components, so `a=1&&b=2&` came back as
`a=1&b=2` — equivalent to every parser that will read it, and still a rewrite of a
destination that needed no rewriting, on the path where nothing collided at all.

When no pre-existing name decodes to a UTM key, the original `rawQuery` is now used
as written with the UTM suffix appended. The split runs only on the collision path,
and preserves empty components there too, since an empty component cannot name a UTM
key. An empty input query short-circuits to the suffix alone, which is what stops the
non-collision branch emitting a leading `&`.

## 4. The round-8 mutation counter was handler state read without synchronisation

`TestCreateCampaign_RefusesACredentialFragmentOnTheRegistrationURL` — added last
round — incremented a plain `int` inside the `httptest` handler and read it from the
test goroutine with no happens-before edge. That is
`httptest-handler-state-needs-synchronized-handoff` at its `high` arm, because the
unsynchronised value IS what the assertion depends on. Now an `atomic.Int32`.

## Declined, with reasons

- **Send `as_user_id` in a signed body rather than the request URI.** Fifth time for
  this one. The exposure is real and it is closed at every rendering site we own; what
  remains is a transport change larger than the exposure, touching every signed call.
  It is worth a ticket, not this branch.
- **Rewrite history so the intermediate commit `95a4471` is not shipped.** That commit
  emitted promotable-user IDs into errors; the final state is value-free. Rewriting it
  out is destructive and outward-facing, the branch is unpushed and was never deployed
  — so there is nothing persisted to purge — and the call is the author's, not the
  review's. Raised rather than performed.
- **Mask the host too (`https://xxxxx`).** Fifth time. The KB's verbatim quote is about
  `redactAIProxyURL`, where the INTERNAL proxy hostname was itself the deployment
  secret. A public event-registration host is not, and the host is the load-bearing
  half of what a redacted URL can still tell its reader.
- **`safeQueryKeyForError` echoes credential material (`?access_token_SECRET=x`).** A
  misreading of the code. That branch is guarded by `named[key]`, which means the key
  was written `name=value` — so it IS a parameter name and structurally cannot be the
  secret. The bare-component case the finding describes is exactly what the round-7
  name-vs-category split already routes away from `safeQueryKeyForError`, and what
  fix 1 above extends to the fragment.

## Coverage

Each of the first three fixes was verified by reverting it alone from a scratchpad
copy and re-running:

- Restoring `'` to the stop set fails both new `TestSanitizeSnapshotText` rows, and
  the failure output shows the literal secret surviving:
  `sanitizeSnapshotText(…) = "https://events.example'api_token=SECRET"`.
- Reverting the bare-fragment screen fails
  `TestCredentialFragmentError_ScreensABareFragmentToo` on all four credential-shaped
  fragments while every section anchor still passes — so the test measures the screen,
  not the refusal rate.
- Reverting the reassembly fix fails
  `TestAppendUTMToRawQuery_DoesNotReassembleANonCollidingQuery` with
  `a=1&b=2&utm_…`, on both the non-colliding and the colliding arm.

Fix 4 is stated honestly rather than claimed as verified: reverting it to a plain
`int` does NOT trip `-race`, because the handler only writes the counter on a POST and
the test asserts no POST ever arrives — so the write never executes and there is no
race to observe. The race is latent, and it is exactly the regression the assertion
exists to catch: if the screen ever stops running first, the POST lands, the write
happens, and the racing read is the thing reporting it. Synchronising it prospectively
is what the pattern asks for.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the bare
fragment, and reassembly), `docs/knowledge/code/internal-dispatch.md` (how the stop
set is chosen). `docs/api-catalog.md` updated for the non-reassembled query and the
bare-fragment screen.

Refs: LFXV2-2665

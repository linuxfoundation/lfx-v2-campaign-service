# 2026-10-26 — the log's kind marker is now checked, not just documented (#176)

**Fix** — `validateLogFragment` returned as soon as the H1 validated, so CLAUDE.md's rule
that an entry opens with a bold kind marker was guidance only: nothing rejected an entry
that opened with prose, and nothing rejected a marker CLAUDE.md does not list. It now
checks the marker against a closed set of the six CLAUDE.md names (`Update`, `Fix`,
`Creation`, `Note`, `Verification`, `Docs`), naming the offending value and listing the
allowed ones rather than only refusing the line.

The set is closed deliberately. The marker is what classifies an entry for a reader
scanning 900-odd fragments, so a plausible one-off (`Feature`, `Tests`) is not a richer
label — it is an entry that sorts under nothing. That is exactly what the sweep found.

**The blast radius was measured before the check was written, not read off the issue.**
914 fragments, 8 non-conformant — and three of those eight were not defects but shapes the
first draft of the rule did not know about:

- A **superseded** entry carries a `> **Superseded …**` blockquote ABOVE its marker, so a
  reader meets the warning before the entry it qualifies. The banner is itself bold, so a
  naive "first non-blank line" check both misses the real marker below and rejects the file
  for a word (`Superseded`) never meant to be one. `nextMarkerCandidate` skips blank lines
  and a leading blockquote; the marker below the banner is still checked.
- An **archive** rolls many entries under one H1, each behind its own `##` heading with its
  own marker — the single archive file holds 34, all already conformant. Applied at the top
  of the file the check would find a `##` heading where it wanted a marker and reject every
  archive; skipping archives outright would leave the bundle's largest files unchecked. It
  descends per section and names the section in any failure, because a path alone does not
  say which of 34 entries to go and fix. The shape is detected by **structure** — what
  follows the H1 — rather than by filename, since a name convention would be a second rule
  to keep in sync.

The five real defects were corrected in the same change: `Tests` and `Test` →
`**Verification**`, a missing marker on the Reddit review-followups entry → `**Fix**`, and
two `Feature` markers → `**Creation**` (three new endpoints) and `**Update**` (criteria
*gaining* fields — an extension, not a creation). Picking the accurate marker rather than
the convenient one is the point of the rule.

**Verification** — three mutations, each reverted, each killed by the test written for it:
neutralizing `checkKindMarker` fails all four negative tests; removing the blockquote skip
fails both banner tests and the real-bundle test; disabling archive detection fails the
archive test and the real-bundle test. Every assertion names the offending marker in the
error TEXT rather than only counting errors — a fixture written to exercise the marker can
otherwise fail on its filename or date instead and stay green with the check deleted.
`TestValidateLogFragmentAcceptsEveryDocumentedKind` pins the validator's set to CLAUDE.md's
in the other direction, so adding a marker to one list and not the other fails too.

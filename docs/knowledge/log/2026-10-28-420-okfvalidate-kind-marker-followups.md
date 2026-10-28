# 2026-10-28 — okfvalidate: bind the kind-marker set to CLAUDE.md, skip fenced archive headings, test the no-body branch

**Fix** — three review findings left unresolved when #401 merged, raised as issues rather
than hot-fixed (#420, #421, #422).

**#420: the drift guard now reads CLAUDE.md.** The #176 entry said
`TestValidateLogFragmentAcceptsEveryDocumentedKind` pinned the validator's set to CLAUDE.md's
"so adding a marker to one list and not the other fails too". It did not: it looped over a
hard-coded third copy of the six names, so only removing a kind from the validator would fail
it. It is replaced by `TestValidateLogFragmentKindsMatchClaudeMD`, which takes CLAUDE.md's
knowledge-log step (the numbered item that names `docs/knowledge/log/`) and collects every
backticked bold word in it. It compares that set to `logFragmentKinds` in both directions,
then checks that each documented kind is accepted. Adding a kind to either list alone now
fails the test. That makes the #176 entry's claim true, and that entry is not edited.

**#421: fenced `##` lines are not archive sections.** Archive mode treated every trimmed
`## ` line as a section heading, including one inside a code fence. So an entry that quoted
a sample log got a spurious "not a bold kind marker" error. A `##` line inside a fence is
no longer a section, and a section after a closed fence is still checked. The fence
handling itself is now done by a CommonMark parser, described in the #426 entry
(`2026-10-28-426-okfvalidate-commonmark-parser.md`).

**#422: the "has no body" branch is tested.** `checkKindMarker`'s `!ok` path had no test.
New tests cover three single-entry fragments (an empty body, a body that is only a superseded
banner, and blank lines only) and an archive whose last section is empty. Each asserts on
the error text, so deleting or rewording the branch fails a test.

**Review round on the PR.** Reviewers on the PR found more ways a fence lookalike could
still hide later archive sections: indented code, an inline code span that opens a line,
and fences inside list items and blockquotes. Rather than patch a line scanner case by
case, the PR replaced it with the parser described in the #426 entry, and each lookalike
has a test.

A top-level fence still open at the end of an archive is now reported as an error instead of
being trusted, so a stray fence can never turn the rest of the file into a silent pass. The
CLAUDE.md drift test also captures the whole bold label, so a documented `**Bug-Fix**` fails
the match instead of being skipped. Each new test was checked by reverting the line it
guards.

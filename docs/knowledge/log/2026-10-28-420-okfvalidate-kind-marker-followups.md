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
a sample log got a spurious "not a bold kind marker" error. `validateLogFragment` now tracks
fence state through `fenceState`. A run of three or more backticks or tildes opens a fence.
The fence closes only on the same character, with a run at least as long and no info string.
Section matching is skipped while a fence is open. Two tests cover this: a fenced heading is
ignored with either fence character, and a section after a closed fence is still checked. In
the second test, a tilde line inside a backtick block does not close the block.

**#422: the "has no body" branch is tested.** `checkKindMarker`'s `!ok` path had no test.
New tests cover three single-entry fragments (an empty body, a body that is only a superseded
banner, and blank lines only) and an archive whose last section is empty. Each asserts on
the error text, so deleting or rewording the branch fails a test.

**Review round on the PR.** Reviewers on the PR found three ways a fence lookalike could still
hide later archive sections:

- a backtick line indented four or more spaces, which is indented code, not a fence;
- an inline code span that opens a line, such as ```` ```go test``` runs it ````;
- a fence inside a list item (`- ```markdown`), which the trimmed match missed.

Fence detection now strips any blockquote or list-item prefix, allows at most three spaces
of indentation, and refuses a backtick opener whose info string contains a backtick. A
section heading likewise counts only when it is indented by at most three spaces, as for any
CommonMark ATX heading.

A fence still open at the end of an archive is now reported as an error instead of being
trusted, so a stray fence can never turn the rest of the file into a silent pass. The
CLAUDE.md drift test also captures the whole bold label, so a documented `**Bug-Fix**`
fails the match instead of being skipped. Each new test was checked by reverting the line
it guards.

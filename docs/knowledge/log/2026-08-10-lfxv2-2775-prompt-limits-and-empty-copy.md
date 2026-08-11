# 2026-08-10 — LFXV2-2775: why the prompt's limits are tighter than the schema's

**Update** — the two numbers in `composeEmailCopyPrompt` that look like a bug are deliberate, and
the guard that catches a model ignoring them now has a test.

## The apparent mismatch

The system prompt tells the model `max 60 chars` for the subject and `max 100` for the preheader.
`design/brief.go` bounds those fields at 200 and 150, and `parseEmailCopyResponse` truncates to the
same. Read side by side that looks like one of the two was never updated, and the obvious "fix" is
to raise the prompt to 200/150.

It is not a mismatch, it is headroom. A subject line is cut off around 60 characters in most inbox
listings and a preheader around 100, so copy written to the SCHEMA's limit is copy the recipient
never sees the end of. The prompt aims the model at the length that actually renders; the schema
sits above it so an overrun is trimmed rather than rejected. Collapsing the two would produce
valid, stored, invisible copy.

`composeEmailCopyPrompt` now carries that reasoning in a doc comment, ending with the instruction
not to raise the numbers: if they ever move, the question is what renders in an inbox, not what the
type allows.

## The incomplete-copy guard

`GenerateEmailCopy` has three distinct 503s — model unreachable, response unreadable, and copy that
parsed cleanly but left a required field blank. Only the first two had tests. The third is the one
a model actually trips: `{"subject":"","preheader":"x",…}` is well-formed JSON, so
`parseEmailCopyResponse` returns no error and the blank field reaches the caller unless the
emptiness check catches it.

`TestGenerateEmailCopy_RejectsIncompleteCopy` covers it, one sub-test per field rather than one
case blanking the subject. The guard is a single `||` chain and a one-field test would still pass
if three of its four terms were lost. Verified binding: deleting the guard fails all four
sub-tests, each naming its own field.

The test also asserts the message string, not just the error type. All three 503s share
`*briefs.ConnServiceUnavailableError`, so the message is the only thing that tells an operator
which failure they are looking at.

## Shared test helper

`strPtr` moved from `orchestrator_test.go` to a new `internal/service/testutil_test.go`.
`connection_test.go` and `email_copy_test.go` both use it now, and Go compiling every `_test.go` in
a package together makes that dependency invisible — nothing in either file says where the symbol
comes from. A file named for the fact is the cheapest way to declare it. Helpers with exactly one
caller stay in their own file.

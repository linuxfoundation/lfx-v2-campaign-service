# 2026-08-24 — a constraint must be stated in the unit of the representation it is published against

**Fix** — creative-asset upload: two review findings on PR #170 turned out to be the same mistake
made in two places — a bound written in the unit of the thing it *meant*, then published against a
representation measured in a different unit.

## The schema case

`design/brief.go` declared the image ceiling as `MaxLength(31457280)` — 30 MiB, the decoded image.
Goa publishes that attribute as `type: string, format: binary` and emits the constraint as
`maxLength: 31457280` on the **JSON string**. `maxLength` on a string counts characters, and base64
expands by 4/3, so the published contract rejected uploads at ~22.5 MiB decoded
(`31457280 / 4 * 3`) — well inside what the endpoint and the 42 MiB wire cap intend to accept.

Nothing local disagreed, and that is why it survived: the generated *server* validator applies the
same number to the decoded slice, so server and schema were consistent with each other and wrong
together. Only a third party — a standards-compliant validator, or a generated client — would have
seen the contradiction, by refusing a request the server would have honoured.

The fix states each bound in the unit of the representation it is attached to:

- the design declares `MaxLength(41943040)` — the ENCODED ceiling, derived as
  `base64.StdEncoding.EncodedLen(30 MiB)`, because that is what the published schema measures;
- the DECODED 30 MiB ceiling moved to `maxCreativeStoredBytes` in `internal/service`, the only
  layer that sees decoded bytes.

The test derives the expected figure through `base64.StdEncoding.EncodedLen` rather than asserting
`41943040`. A hardcoded literal would restate the constant instead of proving the relationship, and
would still pass if both numbers moved together and became wrong together — which is exactly the
failure mode that hid the original defect.

## The status case

The same PR declared `Response(StatusCreated)` unconditionally on an endpoint that is **idempotent
on `(brief_id, checksum)`**. A re-upload of identical bytes returns the existing row, so 201 told a
retrying client it had created a resource when nothing was created.

The reason it was not caught by any test is worth recording: the returned row is *byte-identical* on
both paths — same id, same metadata, the first upload's `created_at`. A test asserting on the
returned asset passes against the defect. The information simply did not exist above the SQL: the
repository discarded it before the service could see it.

`INSERT ... ON CONFLICT DO UPDATE` can recover it. `RETURNING (xmax = 0) AS inserted` is true for
exactly the rows the statement inserted — an INSERT leaves `xmax` at 0, while the DO UPDATE arm
writes a row version whose `xmax` is the current transaction. It is read on the same `RETURNING` as
the row, so it cannot disagree with what was returned.

That flows out through `CreateAsset(...) (*model.CreativeAsset, bool, error)` to a `created` Tag
attribute, which the generated encoder switches on to emit 201 or 200.

## The generalisable part

Both defects share a shape: **a number was correct about the quantity its author had in mind, and
was then attached to a representation that measures something else.** 30 MiB is a true statement
about an image and a false one about a base64 string; "created" is a true statement about the first
upload and a false one about the row returned to the second.

Neither is detectable by checking the value. Both are detectable by asking *what does the layer this
is published to actually measure* — which is the question to ask of any constraint that crosses a
representation boundary.

A corollary for tests: asserting the attribute is not asserting the status. `created` is a service
field; a client sees a status line, and the mapping between them lives in generated code. A Tag on
the wrong value, or a `Response` ordering that makes 200 unreachable, passes every service-level
test and still ships the defect — so the wire test drives the generated encoder and asserts the
status code itself.

# 2026-08-31 — a fix that inverted its own bug

**Update** — The single-widget guard has now been wrong in both directions, and the second error
was introduced by the fix for the first.

## The undercount, then the overcount

`applyEmailContent` refuses to write a generated body unless a template has exactly one rich-text
widget, because there is no safe way to choose which block a body replaces. It counted
`len(GetEmailHTMLWidgets(...))`, and that map omits widgets whose body trims to empty — so a
template with one populated block and one EMPTY block reported `1`, passed the guard, and had its
body rewritten. That was the undercount, fixed by returning a separate total that counts empty
blocks too.

The fix counted every widget that decoded into `struct{ Body struct{ HTML string } }` — and an
image module decodes into that shape perfectly happily, leaving `HTML` empty. So the ordinary
template (one rich-text block plus a header image) reported **2**, the guard declined, and the
generated body was silently never written. Only an info log said so.

Worse than the bug it replaced: the undercount corrupted rare templates loudly; the overcount broke
the common one quietly.

## What the count actually has to answer

Two questions, and a struct cannot express the difference:

- *Is this a rich-text widget?* → the `html` KEY is present in the body object.
- *Does it currently hold copy?* → that key's value is non-empty.

An image body has no `html` key at all; an empty rich-text body has the key with an empty string.
Decoding into a typed struct collapses both to `HTML == ""`. `widgetBody.Body` is therefore a
`map[string]json.RawMessage`, and `widgetBody.html()` returns `(value, keyPresent)`.

## How to apply

**When a fix changes a count, test the inverse case in the same commit.** The empty-block test
existed and passed throughout; nothing exercised a non-rich-text module, so the overcount shipped
green. Both directions are now pinned — `TestHubSpot_EmptySecondWidgetKeepsItsBody` for the
undercount, `TestHubSpot_HeaderImageDoesNotBlockTheBodyWrite` for the overcount — and reverting
either way fails exactly one of them.

**A `continue` guarded by an unmarshal error is not a type filter.** The old comment claimed "a
widget that isn't this shape (image, divider, …) is not ours to touch", but `encoding/json` ignores
unknown fields and populates nothing, so every object-bodied module passed. A comment asserting a
filter that the code does not perform reads as verification and is not.

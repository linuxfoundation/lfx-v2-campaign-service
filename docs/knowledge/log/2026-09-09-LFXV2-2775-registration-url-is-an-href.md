# 2026-09-09 — LFXV2-2775: the registration URL is an href, not a string

**Fix** — `httpURL` in `internal/service/email_copy.go` accepted three shapes it should not,
each confirmed with a probe before the change:

- `https://:443/path` — `url.Parse` gives this a NON-EMPTY `Host` (`":443"`) and an empty
  `Hostname()`, so the previous `u.Host == ""` check passed a URL with no host at all.
- `https://user:password@host/reg` — embedded credentials. This value is interpolated into an
  LLM prompt and the model is asked to place it in an `href`, so accepting it discloses the
  credential to the model provider and to every recipient of the sent email.
- `https://events.example/" onclick="alert(1)` — `url.Parse` is a STRUCTURAL parser and accepts
  a quote in a path, query or fragment. Returned raw, it closes the attribute in generated HTML.

The first two are now rejected, matching the validator the paid adapters already use. The third
is ESCAPED rather than rejected: the function returns `u.String()` with `RawQuery` re-encoded via
`url.Values.Encode()`, because `String()` percent-encodes the path and fragment but emits
`RawQuery` verbatim — without the explicit re-encode the path fix is cosmetic and a quote in the
query still survives. A malformed percent-escape is refused outright, since `url.Query()` drops
the offending parameter silently and would change the destination.

Accepted cost: `Encode()` SORTS query parameters, so a returned URL can differ from the
operator's in parameter order. Query order is not semantically meaningful to any LF registration
destination, and the value is used for linking, never display.

**Note** — this is the SEVENTH copy of essentially this validator in the repo (googleads,
linkedin, meta, microsoft, reddit, twitter, and now the email path). Each is unexported inside its
own platform package, which is why the email path grew a thin reimplementation rather than a call.
Extracting one shared validator is the right fix and was deliberately not done on a PR scoped to
email copy placement; until it is, a change to any of these rules belongs in all seven.

**Update** — `EmailHTMLBlock` gained a `Placed` field. `htmlBlocks` orders layout-placed blocks by
the drag-and-drop tree and appends the rest in sorted key order, and a caller cannot tell the two
apart from the slice alone. On a CLASSIC template nothing is placed, so `blocks[0]` is whichever
module id sorts first — as likely the footer as the opening paragraph. `Placed` lets a caller that
means "the top of the email" require it instead of trusting an index that is only a key sort.

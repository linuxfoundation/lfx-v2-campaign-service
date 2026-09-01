# 2026-08-29 — three guards that counted, redacted, or reported the wrong thing

**Update** — Bot review on the email-content-apply PR found three defects that share a shape: each
guard ran, and each measured something adjacent to what it was protecting.

## An empty rich-text block is still a block

`applyEmailContent` refuses to write the body unless a template has exactly one rich-text widget,
because there is no safe way to choose which block a generated body replaces. It counted
`len(GetEmailHTMLWidgets(...))` — and that map OMITS widgets whose `body.html` trims to empty.

A template with one populated body and one empty second block therefore reported **1**, passed the
guard, and had its populated body rewritten: exactly the ambiguity the guard exists to refuse. An
empty block is one an operator can see and fill; it is part of the template's structure, not an
absence.

`GetEmailHTMLWidgets` now returns the map AND the true total, because they answer different
questions: *how many blocks are there* must count every block, *which can I write* must not. The
UTM tagger discards the count deliberately — an empty block has no links to tag.

## A bounded search that matched nothing was reporting an absence

`maxFilteredScan` bounds a filtered walk so a rare query cannot run to the request deadline. On
truncation it returned `(out, nil)` — and with `out` empty that states *the portal authoritatively
holds no such email*, about a template that may sit on the next unread page. The endpoint's
published contract explicitly prefers a recoverable 503 to that claim.

The warning it logged reached an operator, not the caller, so it moved the confusion rather than
removing it. `ErrSearchIncomplete` now fires **only for the zero-match case**: with rows in hand a
bound is a partial answer the caller can use, and the listing is documented as bounded. Two
existing tests asserted the old empty-and-nil result — they were pinning the false absence, and
updating them is the visible half of the fix.

## A default-allow redactor on a credential path

`buildLogError` collapsed two credential sentinels and returned everything else raw, reasoning
that whatever else arrived was a HubSpot API error rendering as method/path/status.

That reasoning holds for the errors arriving *today* and is falsified by the next one added:
`AudienceBuilder.client` JSON-decodes the **decrypted** credential blob, and a decode error's text
is shaped by the struct it decodes into — a shape this function does not control and cannot see.
It is now default-DENY, naming the arms whose text is known safe (its own sentinels, context
errors, and `hubspot.IsAPIError`, exported for exactly this: a caller cannot establish that
property about an arbitrary error, so the distinction has to be drawn where the type is known).

**How to apply.** For any guard, ask what it MEASURES versus what it PROTECTS. All three of these
ran on every call and looked correct in isolation; the gap was one step to the side — a filtered
count, a log line the caller never sees, an allow-list that only enumerates today's errors.

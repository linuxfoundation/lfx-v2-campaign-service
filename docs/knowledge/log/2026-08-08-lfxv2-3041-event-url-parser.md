# 2026-08-08 — Parsing hostile markup: bounds, order, and whose field names these are

**Update** — the parser half of `internal/platform/eventurl`. It turns a fetched page into
`EventDetails` via JSON-LD `schema.org/Event`, then OpenGraph, then `<title>` plus
`meta[name=description]`. The fetch half landed separately; the endpoint that exposes both
follows.

## A decoder limit is not a traversal limit

`encoding/json` refuses to decode past 10000 levels of nesting, and that was taken as the
bound on graph traversal. Different quantity: it caps what DECODES, and a document that
decodes fine can describe far more nodes than any real page. The recursive walk also returned
a fresh slice per frame and appended each child slice into its parent, so a nested `@graph`
chain re-copied the accumulated result once per level — quadratic allocation on
attacker-controlled input, plus a goroutine stack frame per level.

The walk is now iterative over a heap stack with explicit `maxJSONLDNodes` and
`maxJSONLDDepth` caps; a malformed document yields the nodes found so far, which the caller
already treats like any page whose Event it could not locate.

**General form: when a limit is inherited from a library, check that it bounds the quantity
you care about.** "The decoder won't let it get that deep" is about decoding, not about the
work done afterwards on what decoded.

## Ordering is load-bearing, not incidental

`setIfEmpty` gives a field to the FIRST node that supplies it, so a page's `Organization`
block cannot overwrite the `Event`'s own name. That rule only means anything if traversal is
in document order — which, with a stack, requires pushing array elements in REVERSE. Get it
backwards and a page listing several events silently binds the brief to the last one.

The invariant now has its own test, because it is invisible: the natural-looking loop is the
wrong one, and every fixture with a single Event passes either way.

## The JSON keys were the contract, and they were wrong

`EventDetails` serialized its name as `name`. Every existing consumer of a brief's
`event_details` blob reads `eventName` — `briefFields` in the Reddit dispatcher and
`briefEventDetails` in the audience builder both refuse a brief without it. Since the endpoint
tells callers to store this result with create-brief, it would have stored cleanly and then
failed campaign dispatch two steps later, with nothing in between saying why.

**A struct returned expressly to be stored does not get to pick its own field names.** They
were already chosen, by whoever reads the store. The keys are now the blob's camelCase set and
a test fails with a diagnostic naming the key.

`URL` is deliberately NOT emitted as `registrationUrl`, though it would have been the easy
match: the dispatchers treat that as the link an ad sends a human to, and an event's landing
page is often not its registration form. Guessing here would be a wrong value rather than a
missing one.

## Degrade, do not guess

Three strategies in strict precedence over ONE parse of the tree — parsing per strategy paid
`html.Parse`, the most expensive step, three times on a body that may be 10 MiB.

JSON-LD's real-world shapes are where a literal reading loses data silently: the root is
commonly an array or an `@graph` wrapper, `@type` may be a string or an array, and a property
like `location` or `image` arrives as a node object rather than a string. Each of those, taken
literally, produces an empty field with nothing saying why — the failure mode that looks like
"this page has no metadata".

Every field is clamped on a RUNE boundary: a byte slice of UTF-8 can end mid-sequence, and
Postgres rejects the invalid string on insert, turning an oversized field into a failed
request rather than a short one.

## Correction, same day — two defects found in review

**Provenance was not all-or-nothing.** All three strategies wrote into one shared
`EventDetails`, so a losing strategy's fields survived into the winner's result. A page with
JSON-LD carrying a description but no name, plus OpenGraph tags carrying both, produced
`extractedFrom="opengraph"` with the JSON-LD description attached. Reverting the fix
reproduces it exactly: `Description = "jsonld description"` under an `opengraph` label. Each
strategy now fills a fresh candidate and only a winner is adopted.

**The JSON-LD media type was compared whole.** `type` is a media type and may carry
parameters; `application/ld+json;profile="..."` is the JSON-LD spec's own form, and a
whole-value `EqualFold` skipped it, degrading silently to OpenGraph or `<title>` with nothing
reported. Now parsed with `mime.ParseMediaType` and compared on the media type alone.

The shared thread between them: **both failures are silent quality losses, not errors.** One
produces a record that is wrong about where it came from, the other a record that is thinner
than the page justified. Neither surfaces anywhere a caller or an operator would see, which is
why both needed a test that asserts the *specific* value rather than merely that parsing
succeeded.

## Three ways the parser picked the wrong node, and they compounded

Review found `@type` matched by substring: `strings.Contains(lower(t), "event")` claims
`EventVenue` (a `Place`), `EventReservation` (a `Reservation`) and the `EventStatusType` /
`EventAttendanceModeEnumeration` enumerations. An `@graph` conventionally lists the venue
BEFORE the event it hosts, so `EventVenue` was routinely the first "match".

Independently, every node wrote into the SAME `EventDetails` through `setIfEmpty`. So the
venue node's `name` was locked in first and the real `Event` node, arriving later, could no
longer supply one. Either defect alone produced a wrong name; together the common case
produced one.

`@type` is now an allow-list of `Event` and its subtypes, normalised across the bare name,
the full IRI and the CURIE spelling. And ONE node supplies every field: `parseJSONLD` commits
to the first NAMED Event in document order. Merging per-field across nodes composes an event
that exists on no part of the page — Event A's name beside Event B's dates, with nothing
downstream able to tell. That is the same all-or-nothing rule already applied between
strategies; leaving it off between nodes left the larger hazard open, since one `@graph`
routinely carries several events while a page rarely carries competing metadata blocks.

The third was `location`. `jsonLDText(ld["location"], "name", "address")` reads only STRING
sub-keys, but the comment beside it presented `address` — which schema.org emits as a
`PostalAddress` node — as the fallback for a `Place` with no `name`. The fallback therefore
resolved to empty for the exact shape it existed to serve. `jsonLDLocation` now handles the
node, JOINING the address components narrowest-to-widest: unlike a name, no single component
stands for the whole, and "San Francisco" is a materially worse venue line than
"San Francisco, CA, US".

## Clamping on a rune boundary is not the same as valid UTF-8

`clamp` cut cleanly, which avoids CREATING invalid UTF-8 and does nothing about what arrives
that way. `html.Parse` does not validate encoding, so a page declaring UTF-8 while emitting
Latin-1 hands us a Go string Postgres will refuse — the oversized-field failure the clamping
was written to prevent, reached by a different door. `sanitize` runs `strings.ToValidUTF8`,
then strips NUL separately: a NUL is valid UTF-8, so `ToValidUTF8` leaves it and a text value
still refuses it. Both run before truncation, since either can change the byte length.

Worth recording which path reaches which pass, because the first test written for this proved
less than it appeared to: the markup path never reaches the NUL strip at all, since
`html.Parse` already replaces a raw NUL with U+FFFD per the HTML spec. JSON-LD is what reaches
it, because `encoding/json` decodes a `\u0000` escape into a real NUL. The test asserts each
pass through the input that actually exercises it.

## What the node and depth caps do not buy

They bound the TRAVERSAL, not peak memory. `json.Unmarshal` materializes the whole value
before `jsonLDNodes` sees anything, so by the time `maxJSONLDNodes` can apply, the allocation
it looks like it prevents has already happened. What bounds that is the fetcher's 10 MiB body
cap, which REFUSES an oversized response rather than parsing a truncated prefix. The comment
now says so, since a reader who mistakes the node cap for the memory bound would be reasoning
from a guarantee that was never there.

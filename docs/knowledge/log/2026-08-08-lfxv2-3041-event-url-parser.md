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

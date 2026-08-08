# 2026-08-08 — The event-URL endpoint, and what it deliberately does not do

**New** — `POST /projects/{projectId}/fetch-event-url` (LFXV2-3043), the third and last
piece of the event-URL chain: `internal/platform/eventurl`'s SSRF-guarded fetch, its
three-strategy parse, and now a route that exposes both. The endpoint fetches an event
page and returns what was extracted from it. It creates and persists **nothing**.

## Typed result, not `Result(Any)`

`Any` renders as `{}` in the generated OpenAPI, so every generated client returns an
untyped value and no consumer can discover or validate the shape. The `event-details`
type names all eight fields, and `extracted_from` is the only **required** one — a record
that cannot say where it came from is not a record worth returning.

Every other field is `omitempty` on the platform side and a **nil pointer** on the wire
when absent, not a pointer to `""`. A form being pre-filled needs to tell "the page did
not say" from "the page said nothing", and only the first should leave the field free to
be authored.

## POST for a read

The method creates nothing, which normally argues for `GET`. The URL decides it: as a
query parameter it is written verbatim into access logs, proxy logs and browser history
at every hop between the caller and this service. As a request body parameter it is not.
That is worth more than the idempotency the verb would have advertised.

## A sibling of /briefs, so it inherits no routing

`/status`, `/metrics` and `/audiences` are all DESCENDANTS of a brief, so they ride the
existing `briefs(/.*)?` HTTPRoute match and the `/briefs/**` Heimdall rule without any
chart edit. This path is a sibling: it needs its own alternation branch in the HTTPRoute
regex **and** its own RuleSet entry, and adding only one of the two produces the two
silent failure modes the chart's `parity_test.go` exists to catch — routed but
unauthorized, or authorized but unroutable. Two rows were added to its curated table: the
accepted path, and `/fetch-event-url/anything` as rejected, since the branch is an exact
alternative with nothing hanging off it.

## NAT64 prefixes are configuration, and the chart test proved it

`eventurl.WithNAT64Prefixes` shipped with the fetch PR as the in-process half of the
answer; this PR is the half that supplies a value. `EVENT_URL_NAT64_PREFIXES` is read in
`pkg/constants`, parsed by `splitCSV`, and applied at the composition root in
`Container.eventFetcher`.

The first attempt wired the constant and not the chart, and
`TestEveryConfiguredEnvVarIsWiredInTheChart` failed with exactly the right sentence: read
by the service, never injected by the chart, *so the feature behind it is silently
disabled in every deployed environment*. That is the whole class — a guard that idles
because the value it guards on is never supplied — and the test caught it before review
did. `WithNAT64Prefixes` panics on a malformed prefix, which is correct **because** the
only caller is the composition root: a prefix typed wrong stops the pod rather than
decoding at the wrong offset for its lifetime.

## The URL that comes back is the page's, not the caller's

`EventDetails.URL` is what the page declares (JSON-LD `url` / og:url); the handler falls
back to the fetched URL only when the page declares none. Callers paste links carrying
tracking parameters, and the canonical is the address an ad should send a human to. The
parser's field comment claimed the opposite — "the page that was fetched" — while no code
in that package ever assigned it; the comment was corrected in the same change, since a
field comment describing a value the package does not produce is how the wrong fallback
gets written by the next caller.

## `mapEventURLErr` default-denies the message, not only the status

Forbidden is `400`, not `403`: nothing about the caller is at issue, and a 403 sends an
operator looking at tokens and roles. A fetch failure is `503` — the closest of the
statuses this method advertises, and it does not blame the caller for a page that was
reachable yesterday.

Matching is by `errors.Is`, because a fetch error wraps both its sentinel and a redacted
cause via multi-unwrap; a type switch would see only the wrapper and send every one of
them to 500. The fallthrough returns a **fixed** string rather than formatting `%v`:
`eventurl` builds URL-free messages precisely because they reach callers and logs, and an
unrecognized error is the one whose contents nothing vouched for. A test pins that by
passing an error whose text contains a token-bearing URL.

## `BriefFromEventDetails` was removed rather than shipped

It was written for this branch and had **no production caller**: the endpoint returns
details for a human to review, and the brief is created through the existing `POST
/briefs` with that payload. Shipping it would have added 71 lines of code plus 363 lines
of test that compile, pass, lint clean and are reachable from nothing — the failure mode
that passes every gate. It lands with the create-from-URL path that calls it, or not at
all.

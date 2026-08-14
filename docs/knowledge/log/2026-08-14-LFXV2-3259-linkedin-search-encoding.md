# 2026-08-14 — LFXV2-3259 LinkedIn search encoding

**Fix** — Every LinkedIn campaign and campaign-group lookup returned
`400 PARAM_INVALID` on fieldPath `search`, so find-or-create could never find an
existing resource and would fall through to create on every call — duplicating
paid campaigns. Found by running a real create against the live Marketing API,
not by review; the third paid-create blocker on this ticket found that way.

**The cause was the space, not the name.** `findMatch` built the Rest.li filter
`(name:(values:List(<name>)))` and handed it to `url.Values.Encode()`, which
renders a space as `+`. LinkedIn's Rest.li parser reads that `+` as a literal
plus inside the `List(...)` literal rather than as a space, and rejects the whole
expression. Every campaign name this client generates contains spaces, so every
lookup failed. The original code comment asserted the opposite — that the
surrounding `url.Values.Encode()` "percent-encodes everything else (spaces,
pipes, etc.) for transport" — which is true for transport and wrong for this
parser: the two encodings are undone at different layers.

An earlier hypothesis, that the pipe and plus in the generated name were the
cause, was **wrong**: a plain name with no special characters at all also 400s.
Pipes do need encoding, but for a different reason and only once the space bug is
out of the way — bare `|` is illegal in a query component and LinkedIn returns
`java.net.URISyntaxException`.

**The fix has two halves, and one alone is not enough.** `restliReplacer` now
emits `%20` for space, `%7C` for `|` and `%2B` for `+` (which would otherwise
decode back to a space). `buildRawQuery` writes `preEncodedParams` — currently
just `search` — to `RawQuery` verbatim, because passing an already-encoded value
back through `url.Values.Encode()` turns each `%` into `%25`, so `%20` becomes
`%2520`. That double-encoded filter matches a literally-different name and
returns a **200 with an empty result set**: a false "not found" that looks clean
and drives exactly the duplicate create the lookup exists to prevent. Keys are
sorted so the query string stays deterministic under Go's randomized map order.

**A test was pinning the bug.** `TestFindMatch_SendsServerSideNameFilter`
asserted that bare `Events | KubeCon` reached LinkedIn, reading the value through
`r.URL.Query()` — which percent-**decodes**, and therefore cannot distinguish a
correctly encoded value from an unencoded one. Any assertion about wire encoding
made through `.Query()` is blind by construction. It now asserts on
`r.URL.RawQuery` via a `rawParam` helper.

**Two payload corrections found while verifying against the live API.**
`costType` is now `CPC`, not `CPM` — the objective is `WEBSITE_CONVERSION`, so
the budget should buy clicks. `offsiteDeliveryEnabled` is now `false`, not
`true`; `true` opts the campaign into the LinkedIn Audience Network, placing ads
on third-party sites, which is a media-buying decision the UI never asks anyone
to make. Neither was pinned by a test, so both could have regressed silently.

**Verified end-to-end** at `LinkedIn-Version 202602`: campaign `868282986` and
group `1191451616` created on ad account `509430019`, read back as `status
PAUSED`, `costType CPC`, `offsiteDeliveryEnabled false`, `servingStatuses
[CAMPAIGN_START_DATE_HOLD, STOPPED, CAMPAIGN_GROUP_START_DATE_HOLD]` — no
delivery, no spend. Each fix was mutation-tested: reverting the space encoding
reproduces the live 400 locally, and reverting the pipe encoding or
`buildRawQuery` each fail with their own diagnostic.

**Still open, and not a code defect.** The dark-post step (`POST /posts`) returns
`403 ACCESS_DENIED`. The token carries the ads scopes but cannot act as
`urn:li:organization:208777` — `/rest/me` is also 403 while `organizationAcls`
returns 200, so organization content scopes (`w_organization_social`) are absent
from the generated token. The campaign layer is complete; the creative layer
needs a token minted with those scopes.

# 2026-08-14 — LFXV2-3259 LinkedIn search encoding

**Fix** — Every LinkedIn campaign and campaign-group lookup returned
`400 PARAM_INVALID` on fieldPath `search`. `findMatch` propagates ANY search
error rather than treating it as a clean absence, so the failure did not
silently duplicate — it **blocked LinkedIn campaign creation outright**: the
live run aborted at the first call, before the campaign group existed. Found by
running a real create against the live Marketing API, not by review; the third
paid-create blocker on this ticket found that way.

(The duplicate-create hazard belongs to the `%2520` double-encoding case below,
which returns a clean 200 with an empty result set — a false absence the caller
resolves by creating. The 400 is the louder failure and the safer one.)

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

**The fix has two halves, and one alone is not enough.** `restliEncode` produces
the final wire bytes for a name (see the next paragraph for why it encodes the
whole query component rather than a character list), and `buildRawQuery` writes
every parameter in `preEncodedParams` to `RawQuery` verbatim. The second half
matters because passing an already-encoded value back through
`url.Values.Encode()` turns each `%` into `%25`, so `%20` becomes `%2520`. That
double-encoded filter matches a literally-different name and returns a **200
with an empty result set**: a false "not found" that looks clean and drives
exactly the duplicate create the lookup exists to prevent. Keys are sorted so
the query string stays deterministic under Go's randomized map order.

`preEncodedParams` is keyed on one property — the value came from
`restliEncode` — rather than on a remembered list of names, because the two are
easy to drift apart (see the second call site below).

**The first fix was an allow-list, and an allow-list was the wrong shape.**
Adding `%20`/`%7C`/`%2B` to an enumerated replacer fixed the names we happened to
generate and missed two characters that truncate the query at the URL layer
rather than inside the Rest.li literal: `&` starts another query parameter (a
lookup for `R&D Summit` searched for `R`) and `#` starts a fragment (`C# Conf`
searched for `C`, with the tail landing in `url.URL.Fragment`). Both silently
produce a false absence — the exact outcome this path exists to prevent. Caught
by Cursor and Copilot independently, which is strong signal. `restliEncode` now
encodes the COMPLETE query component via `url.QueryEscape`, with `+` rewritten
to `%20`; the caller still supplies the `(name:(values:List(` … `)))` syntax
raw. A list needs extending for the next delimiter, and it also left non-ASCII
raw; encoding everything cannot go stale.

**A second `restliEncode` call site had the same double-encoding bug.**
`preEncodedParams` listed only `search`, but `listCreativeURNs` builds
`campaigns=List(<encoded URN>)` the same way, so its `%3A` became `%253A` and the
URN reached LinkedIn as literal text. The finder matched no campaign, and that
empty result reads as "this campaign has no creatives" — so a status cascade
silently skipped every creative. Pre-existing (main emitted the same bytes), but
the same class one function away. Its test asserted only
`strings.Contains(query, "555")`, and a numeric id has no reserved characters,
so the double encoding was invisible to it.

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

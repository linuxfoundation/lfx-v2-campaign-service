---
type: "Code Concept"
title: "internal/utm"
description: "Tags outbound email links with UTM parameters so email traffic is attributable in the warehouse."
resource: "internal/utm"
---

# internal/utm

Rewrites the links in a staged email so its clicks are attributable. Every PAID platform client
builds its own UTM parameters (`metaUTMParams`, `redditUTMParams`, `linkedin.BuildUTMURL`, …);
email had none, so email sessions arrived in the warehouse as direct/unattributed traffic and
the marketing dashboards could not see the channel at all.

## The LF Events convention

`utm_source=email`, `utm_medium=`**`LF-Events`** — medium is NOT `email`. Warehouse channel
reporting keys on this exact pair, so changing either silently re-buckets historical
comparisons instead of failing.

`utm_campaign` resolves in precedence order: the `utmCampaign` on the brief's HubSpot platform
config (an operator's deliberate choice), else a slug of the deterministic email name, else
`email-campaign`. `Resolve` records which was used in `Resolution.Source` — two emails tagged
from different provenances are not comparable in a report, and the difference is otherwise
invisible.

## Rules that exist to prevent silent damage

- **Never double-tag.** A link already carrying a non-empty `utm_campaign` is left exactly
  as-is. Overwriting an author's deliberate campaign produces a URL that still WORKS but reports
  to the wrong campaign — nothing surfaces the loss.
- **Never emit an empty `utm_campaign`.** That is worse than leaving a link bare: the session
  looks tagged while attributing to nothing. Without a campaign, `Apply` returns the URL
  unchanged.
- **Only `http`/`https` links are tagged, and it is an ALLOWLIST.** `isTaggable` was originally a
  DENYLIST of `mailto:`/`tel:`/`#`, which silently let every OTHER non-web action through:
  `javascript:void(0)` — the standard placeholder href marketing tools emit for a no-op link —
  became `javascript:void(0)?utm_source=…`, changing the expression the browser evaluates.
  `sms:`, `data:` and `ftp:` mangled the same way. A link with no scheme (relative `/register`,
  protocol-relative `//lf.dev/x`) is still eligible; a bare `#anchor` is an in-page jump and is
  not. The check runs on the RAW string before `url.Parse`, so a rejected link comes back
  byte-identical rather than round-tripped.
- **Append, never rebuild the query.** `url.Values.Encode()` broke three things at once: it
  REORDERED keys (so a token restored by value could land on the wrong occurrence), DROPPED
  anything it could not round-trip (semicolon-separated params vanished entirely), and
  percent-encoded `{{...}}`. Appending leaves every original byte of the query untouched, so
  only the PATH still needs token restoration. A blank `utm_*` already in the query is stripped
  first, or it would win over the appended real value for a reader taking the first occurrence.
- **Never break personalization.** HubSpot substitutes `{{contact.firstname}}` at SEND time, but
  `url.Parse`/`String` percent-encodes the braces — a tagged link would carry `%7B%7B…%7D%7D`,
  HubSpot would not recognise the token, and every personalized link would break. Tokens present
  in the ORIGINAL url are restored after tagging (`restoreTemplateTokens`) in the PATH and the
  FRAGMENT. The QUERY needs no restore and gets none: it is appended to verbatim, so its tokens
  are never escaped in the first place. Restoring query tokens by value was the unsafe round-trip
  the rule immediately above exists to prevent.
- **Existing `utm_*` pairs are REPLACED, not appended to.** Appending leaves the originals
  first, and `url.Values.Get` returns the first — so a stale `utm_source=facebook` would
  out-rank the appended `email` and the link would attribute to the wrong channel while looking
  correctly tagged.
- **Path tokens are restored by splicing the ORIGINAL path back**, not by replacing occurrences.
  `URL.String()` encodes an already-encoded literal and a live token identically, so a
  replace-by-value restore matched the wrong needle and swapped which occurrence was live.
  The FRAGMENT is restored the same way and INDEPENDENTLY: `URL.String()` escapes it too, so
  `#{{contact.id}}` shipped as `#%7B%7B…%7D%7D`. A url may personalize the path, the fragment, or
  both — gating one on the other would leave the second broken.
  The splice is gated on the two paths being equal, and that comparison folds the SCHEME
  (`sameExceptSchemeCase`): `url.Parse` lower-cases it, so a template written `HTTPS://…`
  otherwise failed the check, skipped the restore, and shipped `%7B%7Bcontact.id%7D%7D` — which
  HubSpot never expands. Only the scheme is folded; host and path case stay significant. The
  spliced result keeps the NORMALIZED scheme — the restore recovers the path, not the casing.
- **Query inspection reads the RAW query, never `url.Query()`.** Go 1.17+ dropped `;` as a
  separator, so `Values` silently DISCARDS semicolon-delimited pairs. A `utm_campaign` hidden in
  one was invisible to the never-retag guard but very much present in the sent URL, so an
  author's hand-picked campaign got replaced and its sibling params dropped with it; in the other
  field order the stale pair survived `stripUTM` and the link shipped with TWO conflicting
  campaigns. `splitQuery`/`rawQueryValues`/`hasUTMPair` split on both separators.
- **`stripUTM` re-emits each kept part with the separator that ORIGINALLY PRECEDED it.**
  `splitQuery` must split on both `&` and `;` to SEE a utm_ pair hidden behind a semicolon — but
  re-joining the survivors with `&` then SHREDDED any non-utm value that legitimately contained
  one: `?sig=a;b;c&utm_term=old` came back as `sig=a&b&c&utm_…`, turning one signature into three
  empty-valued parameters. Signatures, base64 payloads, `redirect=` targets and ad-tracker
  macros all routinely carry unencoded semicolons. Nothing failed loudly — the link still
  resolved, the destination just saw a truncated signature, which is worse than the untagged
  link this package exists to avoid.
  **PRECEDING, not trailing** — the distinction is load-bearing. Keying off the byte that
  *followed* each kept part looks equivalent but lets a survivor inherit a separator belonging to
  a REMOVED pair: `a=1;utm_source=fb&b=2` collapsed to `a=1;b=2`, and since `url.ParseQuery` has
  rejected `;` outright since Go 1.17, that returns an EMPTY map — `b` is lost entirely, not
  merely merged into `a`. A part's own leading byte always belongs to that part, so dropping any
  number of neighbours can never reassign it. Only the removed `utm_` pairs change the string.
- **Query KEYS are percent-decoded before comparison (`queryKey`).** `?utm%5Fcampaign=x` decodes
  to `utm_campaign` for every normal reader and for the analytics backend, but a raw comparison
  saw the literal `utm%5Fcampaign`: the never-retag guard missed it, `Apply` appended a SECOND
  campaign, and the strip left the author's original AHEAD of it — two conflicting values, with
  the author's the one a first-occurrence reader picks. An undecodable key falls back to its raw
  form rather than being dropped.
- **Never mangle.** An unparseable URL, or HTML that fails to parse or render, is returned
  UNCHANGED. A broken link in a sent email is far worse than an untagged one.
- **Tokenize; never round-trip a tree.** Parsing requires a context element, and the HTML spec's
  insertion modes DISCARD content invalid for it — `<tr><td><a …>` parsed in a body context loses
  its row and cell, so re-rendering returned a fragment with the table structure stripped. Email
  HTML is written as table layouts, so that was the common case, not an edge one; a table context
  would only move the failure to non-table widgets. `TagHTMLLinksFrom` rewrites `href` tokens in
  place instead, so every byte OUTSIDE a tagged anchor survives verbatim — malformed markup,
  conditional comments, table structure and all. An untouched anchor keeps its ORIGINAL bytes
  (quoting, attribute order, valueless attributes); only an anchor whose href actually changed is
  re-serialized. That re-serialization NORMALIZES the tagged anchor's start tag — attribute names
  lower-case, values double-quoted and HTML-escaped, and a valueless attribute gains an empty
  value (`download` → `download=""`). All are spec-equivalent and parse identically in every email
  client, so the effect is cosmetic — but the byte-identity guarantee covers untouched tokens
  only, and stating it more broadly would invite a future change to rely on identity that does
  not hold.
- **(Historical) `ParseFragment`, not `Parse`.** Email bodies are fragments; `Parse` would wrap them in a
  synthesized `<html>/<head>/<body>` and corrupt the rendered email. The context node's
  `DataAtom` must match its `Data` (`atom.Body`) or `ParseFragment` rejects it and every call
  silently returns the fragment untagged.
- **`utm_content` numbering counts only links actually TAGGED**, so labels have no gaps and
  correspond to what a reader can count in the email.

## Best-effort at the dispatcher

`dispatch.tagEmailLinks` runs LAST, after the clone and the send-list are set, and swallows
every failure with a log. By then the email is a working campaign; failing the dispatch would
turn a reporting gap into a failed send and leave a configured draft behind anyway. It patches
only the named widgets' `body.html`, so untouched widgets and other fields of a touched widget
(styles, module metadata) are preserved.

See [internal/utm](../../../internal/utm).

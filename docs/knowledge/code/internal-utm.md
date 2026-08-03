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
- **Never tag `mailto:`/`tel:`/`#anchor`.** Appending a query string breaks the link.
- **Never break personalization.** HubSpot substitutes `{{contact.firstname}}` at SEND time, but
  `url.Parse`/`String` percent-encodes the braces — a tagged link would carry `%7B%7B…%7D%7D`,
  HubSpot would not recognise the token, and every personalized link would break. Tokens present
  in the ORIGINAL url are restored after tagging (`restoreTemplateTokens`), in both path and
  query positions.
- **Never mangle.** An unparseable URL, or HTML that fails to parse or render, is returned
  UNCHANGED. A broken link in a sent email is far worse than an untagged one.
- **`ParseFragment`, not `Parse`.** Email bodies are fragments; `Parse` would wrap them in a
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

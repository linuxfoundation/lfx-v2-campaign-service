# 2026-11-25 — A newsletter issue keeps its brand's header, footer and links

**Fix** — four defects found by running the newsletter surface end to end against the LF portal,
and by reading how five sent newsletters there are laid out.

- **The brand's footer was replaced.** A newsletter draft is a clone of the brand's previous
  issue, and the full content rebuild writes the standard Linux Foundation Events footer: LF's
  social links and "sent by" line under another brand's newsletter, and the stock footer module
  in place of the brand's own. `hubspot.Client.ReplaceEmailBodyKeepingChrome` keeps the header
  (what opens the email above its first text) and the footer run (from the first social or footer
  module), and replaces only what is between them, one rich-text module per item, without inline
  styles so the draft's own fonts and colours apply. `HubSpotDispatcher.Dispatch` uses it for a
  `newsletter` brief and falls back to the rebuild, with a warning, when there is no footer to keep.
- **No link survived.** The shared prompt's link rules are written for an event email and ask for
  plain text when no Registration URL is given. The newsletter stage now states its own rule in
  the system prompt, and `enforceSourceLinks` holds the answer to it: a link is kept only when its
  whole address is in the brief's notes.
- **The notes lost their lines.** They reach the prompt one item per line now
  (`newsletterFactsBlock`), so an address stays with its item.
- **Events tracking on a brand's newsletter.** Link tagging adds `utm_medium=LF-Events`, which the
  warehouse's channel reporting keys on. A newsletter's links are staged as written unless the
  config names a `utmCampaign`, and then only the issue's own modules are tagged.

Also: `BrandNewsletters` and `NewsletterBrands` count only sent ISSUES (`isSentIssue`). A sign-up
form's automated follow-up and a published send marked as a test in its own name were both listed
as newsletters; the second would have made a test list the next issue's audience.

Verified live on `tlf`: brief `c358ceba` (OpenSSF, a test issue) staged draft 228619151404 with the
OpenSSF logo, nine content modules carrying the eight links from the notes, and OpenSSF's own
social links, "sent by" line and footer module. Unsent.

Concepts updated: `code/internal-platform-hubspot.md`, `code/internal-dispatch.md`,
`code/internal-service.md`. Tests: `content_chrome_test.go`, `hubspot_newsletter_test.go`,
`audience_newsletters_issue_test.go`, `email_source_links_test.go`.

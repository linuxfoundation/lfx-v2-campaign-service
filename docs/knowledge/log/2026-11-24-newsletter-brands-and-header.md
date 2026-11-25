# 2026-11-24 — Newsletter brand picker and the template's header

**Update** — two changes from the first live run of the newsletter surface on tlf.

- `GET /projects/{projectId}/audience-builder/newsletter-brands` lists the brands that have sent
  a HubSpot newsletter (`AudienceExplorer.NewsletterBrands`, name rule
  `dispatch.NewsletterBrandFromName`), most recent first, for the UI's brand dropdown. It walks
  the portal's email history, about 40 seconds on the LF portal, so the listing is cached per
  project for 15 minutes. Routed in the HTTPRoute regex and the RuleSet.
- A staged newsletter draft lost the previous issue's logo header: `RebuildEmailContent`
  replaces the whole widget tree and only adds a header when the brief has a hero image, which a
  newsletter brief never has. For a newsletter brief with no hero, `HubSpotDispatcher.Dispatch`
  now reads the clone's leading header image (`hubspot.Client.LeadingHeaderImage`: the image
  above the first rich-text block) and re-uses it as the hero. The newsletter prompt also opens
  the issue with an `<h1>` naming the newsletter and the issue, as the previous issues do.

See [internal/service](../code/internal-service.md).

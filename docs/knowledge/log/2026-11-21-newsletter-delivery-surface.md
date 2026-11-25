# 2026-11-21 — A newsletter delivery surface

**Creation** — a brand's recurring HubSpot newsletter had no home in the campaign flow; operators
cloned last month's issue by hand. The legacy emailcreationskill found the clone source by name
(`lookup_brand_history` with the hint "newsletter").

- `newsletter` is a fourth `delivery_type`, with one stage, `Newsletter`. Migration `000059` widens
  both CHECKs and is expand-only.
- `GET /projects/{projectId}/audience-builder/brand-newsletters?brand=` returns the brand's previous
  sent newsletters with their lists (`AudienceExplorer.BrandNewsletters`, last-sent's send gates,
  name rule `dispatch.IsBrandNewsletter`). Routed in the HTTPRoute regex and the RuleSet.
- Copy for a newsletter brief comes from the operator's notes (`event_details.sourceText`) and is
  styled on the issues they chose (`event_details.referenceEmailIds`, read by
  `EmailReferenceSource.BuildReferenceBlockFromIDs`, first id = template).

Not yet verified against a live portal: the name rule on real newsletter names, and whether
`RebuildEmailContent` fits a newsletter template's module layout.

See [internal/service](../code/internal-service.md).

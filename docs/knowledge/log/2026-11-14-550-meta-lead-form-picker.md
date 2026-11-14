# 2026-11-14 — #550 Meta Instant Form picker read (gap 8)

**Creation** — The picker read for gap 8 of the Meta gap review (#550). `metaConfig.leadFormId`
takes an existing Instant Form id, and nothing let the UI list them. `GET /projects/{p}/meta-ads/
lead-forms` (`list-meta-lead-forms`) reads the connection Page's `leadgen_forms` edge
(`fields=id,name,status`, the opaque cursor followed for at most ten pages of 100) and returns
`{page_id, forms: [{id, name, status}], form_count}`, sorted by name. Every status is listed; only
ACTIVE passes a create. All or nothing: an unbelievable row or page fails the read, and more than
1000 forms is a 503.

The forms belong to the Page of the connection a create uses, the shared LF connection for every
project without its own, so the read is gated on `global_marketing_ops` (the gateway's
`marketing-ops-api` rule, `TestMetaLeadFormsIsAMarketingOpsRead`, parity rows). Credentials and the
Page resolve as for a create, so a listed id passes the create's own form check.

Behind `META_LEAD_FORMS_ENABLED`, the flag that gates `leadFormId` (400 while off). Not
live-verified: the Page reference documents only POST on this edge; reading it may need a Page
access token with `pages_manage_ads` and `leads_retrieval`.

Tests: `lead_forms_list_test.go` (the walk, eleven unbelievable answers, the empty Page, bad page ids
with no request), `meta_lead_forms_list_test.go` (the connection's Page, local refusals, an upstream
error) and `meta_lead_forms_test.go` in service (sort and mapping, flag and scope refusals, error
classes), the instrumentation row and the chart parity rows. Eighteen hand mutations each fail a
test.

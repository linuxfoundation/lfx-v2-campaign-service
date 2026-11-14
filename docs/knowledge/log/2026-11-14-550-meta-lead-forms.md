# 2026-11-14 — #550 Meta lead forms on the leads objective (gap 8)

**Creation** — Gap 8 of the Meta gap review (#550). The `leads` objective ran as a website-traffic
campaign because the service could not attach an Instant Form. The owner decided (2026-11-14) that
the caller passes an existing form, created in Ads Manager with the Page's lead terms accepted, and
that LFX never creates forms or accepts terms. `metaConfig.leadFormId` with `leads` now creates an
`OUTCOME_LEADS` campaign, a `LEAD_GENERATION` ad set on the Page with `destination_type: ON_AD`, and
creatives whose button carries `lead_gen_form_id`. Without a form, leads keeps the interim traffic
mapping.

The id shape, the objective, the button (LEARN_MORE, SIGN_UP, APPLY_NOW, SUBSCRIBE, DOWNLOAD) and
manual bidding are checked before any request: a synchronous 400, re-checked in the job and in the
client. After the account preflight and before any write the form is read once
(`GET /{form}?fields=id,page,status`); a form of another Page or not ACTIVE is a refusal, a failed
or malformed read an error, and either way nothing is created.

Behind `META_LEAD_FORMS_ENABLED` (default off) until a live probe. Not live-verified: the token's
right to read the form node, the CTA subset, the ON_AD payload and Audience Network delivery.

Tests: `lead_forms_test.go` (the OUTCOME_LEADS payload, leads without a form unchanged, six
pre-request refusals, seven unusable-form answers with no POST) and `meta_lead_forms_test.go` (flag
off and on, a wrong type, manual bidding, Dispatch's wiring and refusal with no request). Seventeen
hand mutations each fail a test.

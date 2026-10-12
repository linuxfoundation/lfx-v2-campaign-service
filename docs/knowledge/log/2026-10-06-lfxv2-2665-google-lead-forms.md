# 2026-10-06 — LFXV2-2665 Google lead form extensions

**Update** — Added LEAD_FORM, the seventh and last ad-extension type this client
creates on Google Search, in a new `internal/platform/googleads/assets_leadform.go`.
It uses the same two mutates every other extension uses — an account-level
`assets:mutate` followed by a `campaignAssets:mutate` link against the resource name
the create returned — so `assets.go` still owns the plan and both mutates; what earns
the file is the shape, because a lead form is not a line of text with a link but a
FORM with a field list, a post-submit screen and a privacy policy.

Two things about it are unlike every sibling:

- **It changes WHERE THE LEAD GOES.** A sitelink sends the user to the advertiser's
  site; a lead form collects their name and email inside Google and holds them for
  retrieval. Attaching one silently changes what "a conversion" means for a campaign
  whose brief assumed site registrations. That is why Google requires the
  privacy-policy URL rather than treating it as optional, and why this client refuses
  a form with no fields instead of creating an empty one — a form that collects
  nothing cannot generate a lead.
- **At most ONE per campaign.** Google links a single lead form, so a second would be
  created as an account-level asset and only then refused at the LINK, leaving exactly
  the "litter rather than a leak" this package already names. `maxLeadFormExtensions`
  is labelled an UPSTREAM limit, not a payload bound, because refusing the second
  locally costs a caller nothing they could have had.

What else the validator refuses: business name (25), headline (30), description (200)
and call-to-action description (30) are all required and bounded in RUNES, the count
every extension here uses rather than the double-width weight `ad_copy.go` applies to
generated RSA copy. Fields are 1..12 and de-duplicate on the input type itself, since
Google renders one input per type and a repeated `EMAIL` is a form that fails upstream
rather than a second box. The post-submit headline and description are ALL-OR-NOTHING
on the sitelink-description precedent — one without the other renders a half-written
thank-you screen the caller cannot see before it is live — while the post-submit
BUTTON stands alone, because Google renders it on its own default screen too.
`callToActionType`, `postSubmitCallToActionType`, `desiredIntent` and every field's
input type are checked for SHAPE only (`enumShapeRE`), on the precedent
structured-snippet headers set. `customDisclosure` is bounded but cannot be fully
checked here: Google only permits it on allow-listed accounts, which this client has
no way to read.

**`validateServableURL` was split out of `buildTaggedFinalURL`.** The privacy-policy
URL has to be held to exactly the same checks as an ad destination — http(s) only,
host required, no embedded userinfo, no malformed query, never echoed back into an
error — but must NOT be UTM-tagged: it is a link Google renders inside the form, not a
destination this campaign claims a click on, so tagging it would attribute a policy
read as an ad click and could break a query the policy host parses itself. Rather than
write a second, laxer URL validator that could drift, the non-tagging half became its
own function, which `buildTaggedFinalURL` now calls and whose parsed `*url.URL` it
reuses so nothing parses twice. The contract for the shared half is unchanged, so no
existing caller's behaviour moved. It is PURE, so Search's preflight stays pure.

**Snapshot.** `config_snapshot` is persisted unencrypted, so `googleAdsSnapshotConfig`
reduces the privacy-policy URL on a copied slice and its early return now names SEVEN
fields rather than six. A shallow copy is enough here — only the price extension's
nested `Offerings` needed the deep one.

**Named gap.** The lead form's optional BACKGROUND IMAGE is not supported, for the
reason IMAGE extensions are not: it carries bytes, so supporting it would give the
Search cascade a network fetch phase it does not have, which is a change to this
package's "Search preflight is PURE" property. A form without one renders on Google's
default background and is servable, not broken. The gap is stated in
`assets_leadform.go`'s header comment and in `docs/api-catalog.md`.

**Parity.** `TestValidateAssetPlan_CountsAndNamesAllSevenExtensionTypes`,
`..._EachNewTypeAloneIsPlanned` and
`TestValidateAssetPlan_RefusesEveryExtensionOnDemandGen` were each widened to seven
types, so a type wired into `validateAssetPlan` but missing from `assetStep` or from
the `asked` sum still fails loudly rather than silently creating or dropping assets.

# 2026-10-02 — LFXV2-2023: Google campaigns can carry a flight window

**Fix** — `googleAdsConfig` gains `startDate`/`endDate` (YYYY-MM-DD, the same spelling
meta/reddit/linkedin/twitter already use), the create payload carries them, and
`applyCampaignConfig` persists them.

Google was the ONLY platform calling `applyCampaignConfig` with `"", ""`:

| platform | passes |
|---|---|
| linkedin, meta, reddit, twitter | `cfg.StartDate, cfg.EndDate` |
| **googleads** | `"", ""` |

So an event campaign with a registration deadline had no way to express it, and the
campaigns table's `start_date`/`end_date` columns stayed NULL for every Google campaign.

**Note** — the surrounding machinery already existed and was waiting for exactly this.
`parseCampaignDate`, the nullable columns, and the settings-readback drift comparison were
all built; the comparison's own comment said a future config populating these "must start
diverging without anyone having to remember to wire the comparison". Only the config field
and the two call sites were missing. That comment is now corrected — this is that future.

**Note** — the v23 field names are NOT the request-side vocabulary a reader would guess,
and getting them wrong fails loudly rather than silently:

- the fields are `start_date_time`/`end_date_time`; `start_date`/`end_date` were REMOVED
  in v23 and are rejected as unrecognized fields
- the format is `yyyy-MM-dd HH:mm:ss`, not the YYYY-MM-DD this service's configs use
- the instant is read in the AD ACCOUNT's timezone, which this client is never told — so
  the value is passed through as wall-clock and never converted to UTC. Converting would
  require guessing the account's zone, and a guess wrong by one zone moves a campaign's
  start or end by a day.

Verified against Google's v23 field reference and their own create-campaign guide rather
than inferred from the readback, which uses the same names for a different purpose.

**Note** — `00:00:00` on the start and `23:59:59` on the end is what makes an end date
INCLUSIVE: "2026-06-20" means through the end of the 20th. Dropping the end to midnight
would silently shorten every campaign by a day, which is why a test pins the exact
suffixes rather than just the date text.

Validation sits in `preflightCampaign`, beside the geo resolution and for the same stated
reason: a malformed or inverted window must be refused BEFORE the budget mutate commits,
or a typo orphans a real paid campaign. Only one ordering rule is enforced — an end before
the start. A start in the past is accepted, because Google accepts one and refusing it
would break creating a campaign whose promotion was always meant to have begun.

**Note** — the window is carried on BOTH channels, and the pre-PR review caught that the
first cut wired only Search. That half-wiring was worse than no feature on Demand Gen:
`applyCampaignConfig` records the window for both channels, so the row would have claimed
an end date the campaign did not have — and because `CompareSettingsField` reports
`unknown` (not `diverged`) when either side is absent, the drift detector this change
re-enabled would have gone quiet exactly where it was needed. Demand Gen builds its own
payload (`demandGenCampaignCreate`) deliberately, since the two channels disagree on
required fields, which is how the field was missed. Google's Demand Gen create guide lists
both dates as optional fields on that channel.

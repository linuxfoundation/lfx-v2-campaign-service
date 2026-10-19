# 2026-10-20 — A survey brief can stage through HubSpot

**Fix** — staging a survey brief failed after every earlier step had succeeded:
`platform hubspot is a email channel, but this brief was authored for survey; create the campaign
from a brief on that surface instead`.

`deliveryChannelKind` is the one place a brief's delivery surface is turned into the channel its
campaigns must use, and `CreateCampaigns` and `AdoptCampaign` both gate every requested platform
on it. It mapped only `email` to the email channel and sent everything else to paid ads, so the
`survey` surface added on 2026-10-16 fell into the paid default and was refused HubSpot, the only
channel it can use. The delivery enum, the column CHECK and the stage pair rule had all been
widened; this mapping was the site that was not.

- `survey` now maps to the email channel. Survey differs from `email` in where its facts come from
  and which stages it offers, not in how it is delivered.
- `AdoptCampaign` uses the same mapping, so this one change also lets a survey brief adopt a HubSpot
  campaign. The other checks on a platform's channel kind (in `brief.go`, `brief_bid.go` and
  `brief_budget.go`) read the EXISTING campaign's platform rather than the brief's surface, so a
  HubSpot survey campaign was already classified as email there and needed no change.
- `TestDeliveryChannelKind` pins the three mappings. `TestEverySendSurfaceAcceptsHubSpot` states the
  property they protect and iterates the delivery types, so a fourth surface added to the enum
  without a mapping fails in a unit test instead of at the staging step.

See [internal/service](../code/internal-service.md).

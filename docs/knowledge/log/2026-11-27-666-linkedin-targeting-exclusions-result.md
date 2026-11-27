# 2026-11-27 — #666 LinkedIn targeting update reads the staff exclusions from the result blob

**Fix** — PR #688 review (Copilot). The targeting update lever read the create-time
`employerExclusions` from `config_snapshot`, which `PUT .../campaigns/{id}` lets a caller replace, so
a `config: {}` edit made the next update drop the staff exclusions silently. `campaignFromLinkedIn`
now records them, non-nil, in the service-owned result blob (`CampaignResult.EmployerExclusions`, the
same place #582 keeps the objective for the same reason), and the lever reads them only from there.
A row that does not record them (created before #666) is refused 409. The snapshot is now only
rewritten, never trusted. Pinned by
`TestLinkedIn_WriteTargeting_ExclusionsSurviveACallerReplacedSnapshot`. Also from review
(Greptile): `LinkedInTargetingUpdate` and `geo_targets` carry explicit valid examples.

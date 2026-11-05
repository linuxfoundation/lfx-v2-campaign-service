# 2026-11-04 — #475 Display audience exclusions and ad-group audiences gated behind the Display audiences flag

**Update** — #475 added two kinds of input that name an account-owned user list or custom
segment: `excludedAudienceSegments` (user lists excluded from the whole Display campaign, or
from one `displayAdGroups` entry) and the audience lists on each `displayAdGroups` entry. The
client checks every one of them against the Google Ads account only. The LF account is shared
by every foundation, so a project could target or exclude with another foundation's user list
or custom segment (#468). #462 had already gated the campaign-level lists behind
`GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED`.

The same flag now covers the #475 inputs. Unless it is exactly `"true"`, `Dispatch` refuses
each of these before the budget, as not-created, through `googleAdsInputDisabled`:

- `excludedAudienceSegments`, non-empty, on any channel;
- on any `displayAdGroups` entry, a non-empty `audienceSegments`, `customAudiences`,
  `customAffinities` or `excludedAudienceSegments`.

`googleAdsDisplayAudienceFieldsSet` names each one in the refusal, a group's by its 1-based
position (`displayAdGroups entry 2 excludedAudienceSegments`), so the sentence carries no
caller value. The group list replaces the single ad group, so without the per-group check a
caller could move a gated list into a group. An exclusion is gated as well as targeting
because the user list it names is account-owned either way.

Not gated, at either level:

- `userInterests`: Google's own affinity and in-market categories, the same ids on every
  account. The campaign-level field was already ungated, and a group's matches it.
- A group's `name` and `creative`: text and the caller's own images.
- `excludedPlacements`: public domains, unchanged.

A `displayAdGroups` config without the gated lists still works with the flag off.

Tests: `TestGoogleAds_DisplayAudienceExclusionsGate` refuses each of the five fields under
unset, `""`, `"false"`, `"TRUE"` and `" true"`, before the budget, with the gate's sentence as
the job message. `..._GateOpensOnExactlyTrue` shows every gated list reaching the client's
preflight. `..._GateLeavesUngatedGroupInputsAlone` shows a group with only a creative and user
interests passing with the flag off. Six mutations of the gate each fail at least one test:
dropping the campaign exclusions, the group exclusions or the group affinities, skipping the
group loop, parsing the flag loosely, and gating a group's `userInterests`.

README, `deployment.md`, the chart comment, `api-catalog.md` and both code pages state the
wider scope. The chart default stays `"false"`; ops enables it per environment by setting
`app.environment.GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED.value` to `"true"` in the ArgoCD overlay.

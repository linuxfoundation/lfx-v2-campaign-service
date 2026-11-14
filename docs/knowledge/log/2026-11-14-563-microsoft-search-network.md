# 2026-11-14 — #563 Microsoft search network (searchPartners) at create

**Update** — Gap 11 of the Microsoft Advertising gap review (#563) adds
`microsoftConfig.searchPartners`, a boolean sent as the new ad group's `AdGroup.Network`: true
includes Microsoft's syndicated search partners (`OwnedAndOperatedAndSyndicatedSearch`), false keeps
the ads on Microsoft's own sites (`OwnedAndOperatedOnly`).

- Omitted, no `Network` is sent and Microsoft's default applies, as before.
- Set only on an ad group this run creates; a reused ad group keeps its network and a warning step
  says so, as for the flight dates.
- A non-boolean, or the field while `MICROSOFT_SEARCH_NETWORK_ENABLED` is off, is a synchronous
  400 (`invalid campaign config: microsoftConfig.searchPartners: ...`). The chart sets the flag to
  `"false"`. Not role-gated.

Not verified live: no Microsoft call was made. The shape is from the Campaign Management v13
`AdGroup` reference.

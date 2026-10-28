# 2026-10-27 — The send-time standing check reads only a one-list send (LFXV2-2770)

**Fix** — follow-up to 2026-10-27-LFXV2-2770-pr-396-review-16, which refused any send to more than
50 lists that excluded the standing list: excluding it is the common case, so a large attach (up to
200 include lists) became unsendable. `audience.StandingCoversSend` now checks every send list
directly with no read (that covers an attach, which sends to its source lists) and reads a filter
only when the send has exactly one list, the shape of every composed audience: the recorded row
has no compose-or-attach marker. `MaxSendListFilterReads` and the size refusal are gone; a failed
read still refuses. Tests (mutation-checked): `TestStandingCoversSend` (a 200-list attach proceeds
with zero reads, a covered list among them is caught directly),
`TestHubSpot_DispatchMultiListAttachExcludingTheStandingListNeedsNoReads`, and the master cases.

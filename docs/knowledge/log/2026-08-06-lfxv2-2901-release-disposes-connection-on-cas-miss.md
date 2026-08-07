# 2026-08-06 — LFXV2-2901: release the connection even when the map CAS misses

**Fix** — Cursor Bugbot finding on PR #78's `ReleaseCampaignLock`: the
`CompareAndDelete` failure branch (a newer claimant already overwrote the map
entry) returned early without disposing `lock.conn`, leaking that checked-out
pool connection for the life of the process — a failed compare-and-delete says
nothing about whether THIS token's own connection still needs releasing, since
nothing else references it. Fixed by making the map bookkeeping
(`CompareAndDelete`) and the connection disposal unconditional-vs-conditional
independently: the map delete only removes this session's own entry (as
before), but `lock.conn` is now always unlocked/released below it regardless of
whether that delete matched. The sibling `ClaimCampaignVersion`'s plain
`Store` overwrite the bot also flagged doesn't need a separate fix — every
successful claim already has an eventual `ReleaseCampaignLock(token)` call
(deferred or cooldown-scheduled) that holds the original `*campaignLock` via
the token's closure, independent of the map's current contents, so this one
fix closes both flagged sites.

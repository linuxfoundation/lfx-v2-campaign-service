# 2026-08-07 — LFXV2-2023: the toggle's account-mismatch guard was undocumented

**Update** — The GA-3c section of `internal-platform-googleads.md` now documents the toggle's
account-mismatch guard, which was implemented but undescribed.

**Fix** — `internal-platform-googleads.md`'s "Status toggling (GA-3c)" section described PAUSE
cascading and the ACTIVATE provisioning gate but never mentioned the account-mismatch guard added
in `370976a`. The omission predates this PR; it surfaced when dealako cross-checked Copilot's
suppressed comments against the code.

The gap matters more than a missing paragraph usually would. The guard is the reason a toggle can
return 409 for a campaign that is fully provisioned — the one 409 cause the section did document —
so a reader working from that section would conclude a 409 means "not provisioned" and go looking
for a missing ad group. The section now states both causes, why the check sits above the
PAUSE/ACTIVATE branch (it is about identity, not direction), how the creation-time customer id is
recovered (`Result.customerId`, falling back to the `googleAdsUrl` customer segment), and that a
campaign carrying neither skips the check rather than failing closed.

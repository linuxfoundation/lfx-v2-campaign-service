# 2026-10-07 — Record the slot-lock deadlock condition correctly

**Docs** — review of #260. The postgres concept and the 2026-10-05 contract entry said the `40P01`
deadlock was observed with the adopt's slot lock removed. Removing that lock removes the adopt's
wait, so there is no cycle. The deadlock belongs to the reverse ORDER: the adopt taking the slot
lock after its brief `FOR UPDATE`, while a claim's INSERT holds `FOR KEY SHARE` on the brief
through the FK and waits on the slot lock. Both passages now state that condition. See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md).

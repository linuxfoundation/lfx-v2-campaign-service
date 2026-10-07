# 2026-10-07 — A brief audience can send to several existing HubSpot lists directly

**Feature** — `attach-existing` accepts `include_list_ids` (1–200 existing HubSpot lists) as an
alternative to `master_list_id`, so an operator can send to several lists they already have
without composing a master list first. HubSpot's `contactIlsLists.include` is an array, so the
lists themselves are the recipients and HubSpot unions them.

- **Exactly one of the two.** `master_list_id` is no longer required; the service refuses both,
  neither, a blank include entry, or an include id that is also a suppression with a `400`
  before any brief read or HubSpot call. Every include list is read back from the portal, so a
  mistyped id is a `404`. The single-master path is unchanged and records no include list.
- **Migration 000040** adds `campaign_audiences.include_list_ids JSONB NULL`, appended last for
  the positional scan. NULL means the master alone is the send set, so nothing is backfilled.
  A multi-include row records `platform_master_list_id` = the first include.
- **The send reads `SendListIDs()`, never the master column.** The HubSpot dispatcher and the
  email wizard both send to every recorded include list; `hubspot.Client.SetSendList` now takes
  `[]string` include ids, de-duplicates them, requires at least one, and refuses any include id
  that is also excluded. The dispatcher's pre-clone overlap check covers the whole set.
- **The master is pinned on a multi-include row.** A `PATCH` changing `platform_master_list_id`
  on a row with include lists is refused `409` even when unstamped, because the column would
  stop matching the send set. `include_list_ids` is response-only on the audience view and the
  attach result.
- **Not changed:** the wizard's explicit `send_list_ids` override still refuses more than one id.

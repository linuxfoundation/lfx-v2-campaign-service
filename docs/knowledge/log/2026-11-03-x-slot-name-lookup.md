# 2026-11-03 — X campaign lookup no longer sends the slot token to search

**Update** — X's `findCampaignByName` filters `GET campaigns` with `q=`, and from multi-campaign
cs-4 a later-slot name carries ` #<n>` in its Targeting segment. Nobody had verified that X's
search matches a `#`. If it does not, a retried slot-2 create would read its own campaign as
absent and create a duplicate, so `twitter-ads` was held out of the multi-campaign rollout.

The lookup now sends `campaignNameQuery(name)`: the name up to its first `#`, trailing spaces
trimmed (no `q` at all if that leaves nothing). Slot-1 names contain no `#`, so their query is
byte-identical. The prefix is a superset of the exact name under prefix or substring search, and
`findByNameChecked` still compares names exactly and refuses duplicates, so the slot's own
campaign is picked out and slot 1's is not reused. Line-item and media lookups are unchanged:
their names carry no slot token.

`TestFindCampaignByName_SlotTokenNotSentToSearch` models a search that never matches `#` and
fails without the change.

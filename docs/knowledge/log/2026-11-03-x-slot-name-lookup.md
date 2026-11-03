# 2026-11-03 — X name lookups no longer send '#' to search

**Update** — X's `findCampaignByName` and `findLineItemByName` filter their list with `q=`. From
multi-campaign cs-4 a later-slot campaign name carries ` #<n>` in its Targeting segment, and an
event or project name can carry a `#` of its own (`C# Dev Summit`), which also reaches the
line-item name. Nobody had verified that X's search matches a `#`. If it does not, a retried
create would read its own campaign or line item as absent and create a duplicate, so
`twitter-ads` was held out of the multi-campaign rollout.

Both lookups now send `nameSearchQuery(name)`: the name up to its first `#`, trailing spaces
trimmed (no `q` at all if that leaves nothing). A name without `#` keeps a byte-identical query.
The prefix is a superset of the exact name under prefix or substring search, and
`findByNameChecked` still compares names exactly, refuses duplicates and checks provenance, so
slot 2 finds its own campaign and never reuses slot 1's. The media-library lookup is unchanged:
its names are content hashes.

`TestFindCampaignByName_SlotTokenNotSentToSearch` and `TestFindLineItemByName_HashNotSentToSearch`
model a search that never matches `#`; both fail without the change.

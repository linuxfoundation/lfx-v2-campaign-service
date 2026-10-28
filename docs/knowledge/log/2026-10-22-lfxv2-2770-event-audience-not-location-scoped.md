# 2026-10-22 — event audience is not location-scoped by default

**Update** — an event audience drafted by `custom/interpret` from an event URL no longer defaults
to the host city and country. Its default is this event's own past registrants (never registrants
of other events held in the same place), education enrollees in the brand's courses, and the event
page's visitors. The host city and country are used only when the request or the answers ask for a
location audience. The default request for a URL-only call is now "Plan the default audience for
this event." rather than "the regional event audience", which read to the model as a request for
location expansion.

The past editions stay a service default rather than something the model copies:
`InterpretCustom` now adds `event.PastEditions` for the event's default audience (no location, the
event page's visitors in `page_view_urls`, no speaker or ambassador restriction) as well as for a
regional one, so `capPastEditions` still keeps the 25 most recent with a note. Leaving it to the
model would have bypassed that: the EVENT block lists the editions alphabetically and cut at 1,500
runes, and the parser keeps the first 25 without a note, so the oldest editions would have won.
Speakers-only, ambassadors-only and topic-only education drafts still get none.

Recorded in `docs/knowledge/code/internal-dispatch.md` and `docs/api-catalog.md`; covered by
`TestInterpretCustom_TheDefaultEventAudienceGetsItsPastEditionsNewestFirst`.

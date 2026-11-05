# 2026-11-04 — #477 a reused creative asset id is accepted, not UNCONFIRMED

**Fix** — #480 made a repeated resource id in a confirmed mutate result UNCONFIRMED in every
loop (`resultIDSet.repeat`). That is right for criteria and links, which are created per
operation. It is wrong for creative assets. Google returns the EXISTING asset for a duplicate
asset create (Google Ads Developer Blog, "Changes to handling duplicate assets in the Google Ads
API", 2022-03), so one image used in two slots, such as a square marketing image that is also the
logo, comes back with the same asset id twice. The check then stopped the cascade before the ad
or asset group was created. This was promised on #480's review thread.

`resultIDSet.reuse` accepts a repeat in the creative asset loops when the operation at the id's
first index sent the same content. The asset is referenced at every slot and field-type position
it fills, and its id is reported once. The loops are Display, Demand Gen image, Performance Max,
the Demand Gen video ad's atomic request, Video and the #477 Search assets. A repeat for
different content stays UNCONFIRMED. The criteria and link loops, the Search ads loop and the
extension asset loop keep `repeat`. The Performance Max link count and the Search campaign-link
check were already positional and needed no change.

Tests and mutations are listed under "A reused creative asset is not a repeat" in the googleads
concept.

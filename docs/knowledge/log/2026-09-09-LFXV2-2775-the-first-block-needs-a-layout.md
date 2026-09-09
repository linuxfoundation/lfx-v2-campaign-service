# 2026-09-09 — LFXV2-2775: "the first block" needs a layout to mean anything

**Fix** — `applyEmailContent` (`internal/dispatch/hubspot.go`) wrote the generated body into
`blocks[0]`, justified by a stated contract: the copy is a lede, the top of the email is where a
lede goes, and picking "the longest" or "the one that looks like prose" would be a guess that
moves between templates.

That contract rests entirely on the position being the LAYOUT's. `GetEmailHTMLWidgets` orders
layout-placed blocks by the drag-and-drop tree and appends everything else in sorted key order, so
on a CLASSIC template — no `flexAreas` at all — `blocks[0]` is merely whichever opaque module id
sorts first. It is as likely the unsubscribe footer as the lede.

The write now requires `blocks[0].Placed`. Without a layout there is no top of the email to speak
of, so the draft keeps its template body and says so at INFO — the same conservative answer as the
no-rich-text-block case. The SUBJECT is still set: it addresses the email as a whole and needs no
layout to be unambiguous.

`TestHubSpot_ClassicTemplateKeepsItsBody` pins it, with a fixture whose lowest-sorting module id
IS the footer. Removing the guard fails it with `wrote into "a_footer"` and the footer's html
replaced by the generated body — the production symptom, reproduced.

**Note** — this was found by chasing a review comment further than the comment went. The reviewer
flagged the ordering in the hubspot package; I checked for callers by grepping the TYPE name,
found none, and reported it as a latent trap. The real caller uses the method's return without
naming the type, so the grep missed it and the finding was live in the dispatch path all along.
A caller search keyed on a type name does not find callers that never write the type — search the
FUNCTION name too.

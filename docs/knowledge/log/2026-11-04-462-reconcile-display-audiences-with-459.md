# 2026-11-04 — #462 Display audiences reconciled with #459's Demand Gen Audience

**Docs** — #469 (#459) and #472 (#462) both changed the `audienceSegments` channel
fence. #459 gave Demand Gen its own refusal that names `DemandGenAudience`. #462 made
Display accept user lists as targeting criteria. Both fences hold together: Demand Gen
refuses with the audience-grouped reason, Performance Max and Video refuse with the
generic Search-and-Display reason, and Display accepts.

The "Demand Gen audience and language criteria (#459)" section of
[internal-platform-googleads](../code/internal-platform-googleads.md) said the other
channels "keep their Search-cascade reason". That stopped being true once Display
accepted the lists, so the section now names each channel's outcome. #459's preflight
test asserted that Display refuses user lists. It now asserts the Performance Max and
Video refusals and the Display acceptance.

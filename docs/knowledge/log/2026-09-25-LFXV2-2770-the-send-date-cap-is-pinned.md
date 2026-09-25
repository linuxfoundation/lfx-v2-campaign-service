# 2026-09-25 — the send-date read cap is pinned, and the dash churn is reverted

**Fix** — `maxSendDateReads` bounds the authoritative send-date fan-out, and no test reached
it. `limit + shortlistHeadroom` is what normally sets the shortlist, and at the design's
`Maximum(10)` that is exactly 22 — so the clamp is unreachable through the API, and every
shortlist test used small candidate counts that exercised only the uncapped branch.

It is still worth pinning: it is the ceiling the endpoint's documented cost rests on ("the
worst case is 22 single-email GETs"), and a refactor that raised the headroom, relaxed the
design maximum, or flipped the comparison would silently turn a bounded fan-out into one GET
per candidate against a rate-limited API.

`TestLastSent_TheSendDateReadIsCappedAtItsCeiling` serves 40 dated candidates, counts the
per-email reads, and asserts exactly `maxSendDateReads`. Calling with a limit ABOVE the design
cap is what makes the clamp reachable from a test without weakening the transport validation
that normally prevents it; the test says so, so it is not read later as endorsing an
out-of-range limit. Deleting the clamp fails that test and nothing else.

**Also** — 37 comment lines had an existing Unicode em dash replaced with an ASCII double
hyphen as a side effect of unrelated edits, while untouched code in the same files kept the em
dash. The file's convention is the em dash (64 occurrences against 9 on main), so the churn is
reverted rather than standardised the other way. Dash-only changed lines: 37 → 0.

A 151-character comment line that a merge had joined mid-sentence is rewrapped.

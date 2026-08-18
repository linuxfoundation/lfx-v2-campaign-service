# 2026-08-18 — LFXV2-3279 only sixteen keyword ids survived the decode

**Fix** — a bound sized for one thing, reused for another.

`AddKeywords` responses decoded `KeywordIds` through `boundedNumberIDs`, whose retention limit is
`maxDecodedErrorItems` (16). That bound is correct for what it was written for: a campaign create
sends ONE campaign, so one id is all that can matter, and 16 is a generous OOM guard for a
malformed null-padded error array. Its own doc comment says so — *"A create sends ONE campaign, so
only the first id is ever meaningful."*

Keywords are not campaigns. `AddKeywords` sends up to `maxKeywords` (60) and **every** id is
load-bearing: it is what the status cascade enables on ACTIVATE. A 60-keyword response therefore
decoded short by design, and the cardinality check *lowered its own expectation to match* —
`if want > maxDecodedErrorItems { want = maxDecodedErrorItems }` — so a short response was
accepted as correct.

The consequence is the part that matters: 16 keywords persisted, ACTIVATE enabled those 16, and
44 stayed Paused on a campaign the operator believes is fully live. A campaign serving on a
quarter of its keywords looks like a targeting problem, not a decode bug.

`boundedKeywordIDs` keeps the streaming shape — a malformed body still must not materialise in
full — and bounds retention by `maxKeywords` instead. The cardinality check now requires one id
per keyword sent, with no lowering, so a genuinely short response is refused rather than
absorbed.

**Found by copilot on #138**, as a suppressed comment. Worth recording how it was nearly missed:
an earlier review pass tested the *validation* path, saw 60 keywords accepted and 61 rejected,
and concluded the "16" claim was false. It was testing the wrong end — the cap was in the
response decode, not the input validation.

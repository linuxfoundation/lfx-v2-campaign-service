# 2026-08-31 — LFXV2-2775: a 503 that promised transience

**Fix** — the composed-prompt overflow returned `"email copy generation is temporarily unavailable
for this stage"`. The code comment two lines above says the opposite: this branch is unreachable by
caller input, so if it fires a service-owned stage template has outgrown its budget — a compiled-in
template exceeding a compiled-in bound. Every retry returns the same 503 until a corrected
deployment ships.

503 already carries "retry later" by convention, so only the TEXT can withdraw it. "Temporarily"
reinforced the wrong reading and would send a caller into a loop that cannot succeed.

The message now says `"...unavailable for this stage; retrying will not help until this service is
fixed"`.

**The test gap this exposed:** `TestGenerateEmailCopy_ComposedBoundIsReachable` asserted the error
TYPE (503 vs the pre-check's 400) but never the message, so the guidance was unpinned and could
regress silently. It now asserts both that the text does not contain "temporar" and that it does
say a retry will not help. Reverting the wording fails it on both.

**The general shape:** where a status code carries conventional advice, an error's TEXT is the only
place a service can contradict it — and a test that pins the type is not pinning the advice.

Related: `docs/knowledge/log/2026-08-31-LFXV2-2775-the-helper-was-tested-the-wiring-was-not.md`.

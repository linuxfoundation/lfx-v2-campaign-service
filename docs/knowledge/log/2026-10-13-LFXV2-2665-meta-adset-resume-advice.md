# 2026-10-13 — Operation-specific advice when a Meta ad-set record is unconfirmed (LFXV2-2665)

**Fix** — After a confirmed ad-set RESUME whose record clear could not be confirmed,
`adSetRecordErr` told the caller "if it is not shown" (the pause advice), and in other branches
that a campaign resume "leaves this ad set paused". For a resume, an absent `deliberately_paused`
means the clear succeeded; only a flag still true is a stale hold over an ad set that is now
ACTIVE, and while it stands a campaign resume is refused with 409. Every branch (lost claim,
unknown write, failed write, row changed) now ends with operation-specific advice from
`adSetRecordAdvice`. After a pause: re-read the flag and pause again if it is absent. After a
resume: re-read it and resume the ad set again if it is still true. The resume message no longer
says the ad set is paused. `TestAdSetRecordErr_AdviceIsOperationSpecific` pins the exact texts for
both operations in both unconfirmed branches. See
[Meta Ad-Set Monitor and Pause/Resume](../architecture/meta-ad-sets.md).

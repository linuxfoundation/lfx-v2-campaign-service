# 2026-08-05 — LFXV2-2901: claim RETURNING, and validate before claiming

**Fix** — Three correctness issues with the claim-based serialization (PR #78 review).
1. `ClaimCampaignVersion` now uses `UPDATE ... RETURNING` to atomically read the
   post-update row, preventing a separate `GetCampaign` re-fetch from being
   interleaved by concurrent writers and returning stale/unclaimed versions.
2. `UpdateCampaign` validation (status mismatch check) now happens BEFORE
   `ClaimCampaignVersion`, so a rejected request (400) does not bump the version.
   Previously, validation after claiming meant a retry of the same rejected
   request would get 412 PreconditionFailed instead of the original error.
3. Documented a known limitation: `ClaimCampaignVersion` provides serialization
   only for concurrent callers reading the SAME version. If caller A claims and
   then makes a long platform call (e.g., toggle to ad platform), caller B can
   read the newly-bumped version DURING A's call, claim it, and enter its own
   platform call concurrently. A future fix should use durable in-flight
   ownership (e.g., lease token or explicit "in-flight" status) to prevent this
   scenario; until then, it's accepted as a small-window edge case.

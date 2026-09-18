# 2026-09-17 Email copy reference-block sizing

**Update** — `GenerateEmailCopy` now builds a best-effort style/tone reference
block from a project's own past sent HubSpot marketing emails
(`EmailReferenceSource` in `internal/service/email_reference.go`), injected into
the stage-aware prompt only (never the frozen legacy prompt, per LFXV2-1940).
`maxComposedPromptSize` moved from 9300 to 11700 runes to accommodate the new
`maxReferenceBlockRunes` (1800) floor contributor: `TestComposedBoundClearsEveryStageFloor`
measured the real worst-stage floor (Post-Event, with a maximally-sized reference
block) at 8770 runes, giving a worst valid composition of 11170 runes against the
2400-rune input bound, so the composed bound was set to 11700 to clear it with 530
runes of headroom.

**Superseded within this same change.** The `urgency-fomo` variant block landed
alongside the reference block, and it is a floor contributor for the same reason:
fixed prompt text appended when `emailCopyPromptVars.variant` matches, not caller
input guarded by `maxPromptSize`. `worstStageFloorNamed` now composes each stage
BOTH with and without the variant and takes the max, which moved the worst case
from Post-Event alone (8770) to **Post-Event with the variant appended (11055)**,
2285 runes higher — so `maxComposedPromptSize` is **14000**, not 11700. The
figures in the paragraph above describe the intermediate state and are kept only
to show how the bound was re-derived; `TestConceptDocSizingArithmetic` computes
the live numbers rather than transcribing them, and is the authority here. `docs/knowledge/code/internal-service-email-copy.md` and
`docs/api-catalog.md` were updated to match, including the historical "At 6500"
narrative's remaining-allowance arithmetic against the new floor.

`EmailReferenceSource` is wired into `BriefService` via the same late-bind
setter pattern as `SetLLMClient`/`SetCreativeAssetRepo` (`SetEmailReferenceSource`),
and into both container startup paths (`wireLiveBackends` and
`retryDatabaseInit`) through the shared `bindBriefLiveBackends` helper, so a
cold-started pod cannot bind the brief repos while forgetting the reference
source.

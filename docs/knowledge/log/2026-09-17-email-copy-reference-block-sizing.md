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
runes of headroom. `docs/knowledge/code/internal-service-email-copy.md` and
`docs/api-catalog.md` were updated to match, including the historical "At 6500"
narrative's remaining-allowance arithmetic against the new floor.

`EmailReferenceSource` is wired into `BriefService` via the same late-bind
setter pattern as `SetLLMClient`/`SetCreativeAssetRepo` (`SetEmailReferenceSource`),
and into both container startup paths (`wireLiveBackends` and
`retryDatabaseInit`) through the shared `bindBriefLiveBackends` helper, so a
cold-started pod cannot bind the brief repos while forgetting the reference
source.

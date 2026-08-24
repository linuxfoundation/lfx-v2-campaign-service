# 2026-08-24 — a correct copy defended by an impossible reason

**Docs** — `resolveVariantAssets` returns a COPY of the variants, and three places explained
why with a claim that cannot be true: that an in-place write would put multi-megabyte image
bytes into the persisted `config_snapshot`. It could not. `meta.AdVariant.ImageBytes` is
tagged `json:"-"` (`internal/platform/meta/client.go:2469`), as is `ImageMIME`, so the
marshal that produces `config_snapshot` omits the resolved bytes no matter what the slice
holds. The stated failure mode was unreachable.

The behaviour is right and is unchanged. The copy exists for CALLER ISOLATION: `cfg.Variants`
is read again after resolution — by `campaignFromMeta` for the config snapshot and by
`Dispatch` for the degraded-ad count — and the variants slice shares its backing array with
the decoded config, so an in-place write would hand those readers a config that no longer
matches what the caller sent. That is a real property, and it is what
`TestMeta_ResolveVariantAssetsDoesNotMutateCallerConfig` actually proves; only its stated
reason was wrong. Verified by mutation after rewording: replacing the
`make`+`copy` with `out := variants` still fails the test, now with the corrected message.

**Why a false rationale on correct code is worse than none.** It passes review, because the
behaviour under it is right and a reviewer checking the behaviour finds nothing wrong. What
it leaves behind is a constraint the codebase does not have. A later maintainer deleting the
copy would re-derive the stated premise, find `json:"-"` makes it moot, and conclude the copy
is dead — removing a guard whose real justification was never written down.

**Three sites, one class, found only by sweeping.** An earlier round corrected this same
sentence in the PR description alone and did not look for other copies; Copilot then found it
surviving in a test godoc and a knowledge doc, and the sweep for the whole class turned up a
third in the source godoc that nobody had flagged. Corrected together:

- `internal/dispatch/meta.go` — `resolveVariantAssets`' godoc
- `internal/dispatch/meta_test.go` — the test's godoc and its `t.Errorf` failure text
- `docs/knowledge/code/internal-dispatch.md` — the dispatch step-2 narrative

Each rewrite now names the real reason and cites the tag and line, so the next reader
confirms it in one step instead of re-deriving it.

**Nearby claims checked and deliberately left alone**, since they concern a different sink
and are true. `campaignFromMeta` sanitizes each variant's `ImageURL` before the snapshot, and
copies the slice first for the same caller-isolation reason — `ImageURL` has NO `json:"-"`,
so it genuinely does marshal into `config_snapshot`. The image-bytes-in-error-text claims in
`internal/platform/meta/client.go` and `TestUploadImageErrorNeverCarriesTheRequestBody` are
also true: they concern `APIError.Message` flowing into `CampaignResult.Steps`, which is
persisted inside the `result` column (`internal/infrastructure/postgres/campaign_repo.go`
line 508) and is a plain string sink with no struct tag to omit it. The
`json:"-"` argument applies to `ImageBytes` reaching `config_snapshot` and to nothing else.

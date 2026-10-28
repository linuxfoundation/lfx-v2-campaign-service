# 2026-10-28 — LLM requests omit temperature for models that reject it (#397)

**Fix** — AI generation returned 400 in production: argocd pins `AI_MODEL` to
`claude-sonnet-5-5`, the 5-series rejects any `temperature`, and `chatRequest` marshalled one on
every request. `temperature` is now a `*float64` with `omitempty`, set only when
`acceptsSampling` approves the resolved model against the `samplingFamilies` allowlist (Claude
3.x, Haiku 4.5, Sonnet 4–4.6, Opus 4–4.6). Opus 4.7+, every 5-series model, and anything
unrecognised get no sampling parameters. The default model (Sonnet 4) still sends 0.7. See
[internal/platform/llm](../code/internal-platform-llm.md).

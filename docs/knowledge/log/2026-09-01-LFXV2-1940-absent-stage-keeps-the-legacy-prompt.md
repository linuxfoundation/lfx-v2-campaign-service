# 2026-09-01 — LFXV2-1940: an absent stage keeps the legacy prompt

**Fix** — `composeEmailCopyPrompt` returned the stage-aware system prompt for every caller,
resolving an absent stage to Registration Push. That satisfied "falls back rather than erroring"
but broke the acceptance criterion one line below it: *"Existing callers that send no `stage`
produce byte-identical prompts to today"*. A caller that had never heard of stages saw its prompt
grow by the placeholder/OMIT rules and a full Registration Push brief.

An absent or whitespace-only stage now returns `legacySystemPrompt` — a frozen, byte-for-byte copy
of the prompt this service emitted before stages existed — together with the pre-stage user prompt
including its `Create compelling email copy...` trailer. Any non-empty stage, recognised or not,
still resolves through `emailstage.Resolve` and takes the template path, so the fallback criterion
is unchanged.

`TestAbsentStageProducesLegacyPrompt` pins this against the golden constants in
`internal/service/email_copy_golden_test.go`, extracted verbatim from the last commit before
stages existed rather than written out by hand or read back from `legacySystemPrompt` itself — either of those would let the test agree with a drifted implementation. It also asserts
the negative: an explicit stage must NOT produce the legacy prompt, so the branch cannot swallow
stage selection. Both directions were confirmed by mutation.

The `absent falls back` row in `TestGenerateEmailCopy_StageReachesThePrompt` was removed: it
asserted the behaviour the byte-identity criterion rejects. `unrecognised falls back` stays.

The goldens are a `.go` file rather than `testdata/*.txt` because the repo's License Header Check
scans `*.txt`, and a header prepended to a golden file would corrupt the very bytes it pins.

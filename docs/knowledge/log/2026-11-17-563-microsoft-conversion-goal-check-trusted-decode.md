# 2026-11-17 — #563 Microsoft conversion-goal pre-create check refuses duplicated keys

**Fix** — Ported with the conversion goals onto current main, where every Campaign Management
decode in `internal/platform/microsoft/` that feeds an identity or PartialErrors classification
goes through `decodeTrusted` (2026-11-17 trusted-decode entries, #617). `CheckConversionGoals`
still decoded its `GetConversionGoalsByIds` answer with `json.Unmarshal`, so a goal answered
`{"Id":5002,"Status":"Paused","status":"Active"}` decoded last-key-wins to Active, and
`{"Id":9,"Id":5002}` to the goal asked for. It now decodes through `decodeTrusted`
(`identityjson.Check` first). A refusal takes the existing malformed path
(`errConversionGoalLookupMalformed`, "not the documented shape"), so the job fails before anything
is created. The two picker reads already apply the same check inside `decodeCappedList`. Covered
by the new duplicate `Status`, case-variant `status` and duplicate `Id` cases in
`TestCheckConversionGoals_Refusals`, which fail with the plain decode restored.

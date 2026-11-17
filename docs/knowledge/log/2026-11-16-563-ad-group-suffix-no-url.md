# 2026-11-16 — #563 Microsoft ad group suffix refuses a URL

**Fix** — `ValidateAdGroupNameSuffixes` refuses an `adGroups[].nameSuffix` that `redact.SnapshotText` would change, such as a URL or credentials. The suffix is stored in the campaign result, the steps and the composed ad group name, and logged as given, so a URL carrying a secret would otherwise be persisted unredacted (#613 review). A suffix is a label; the refusal message does not echo the value.

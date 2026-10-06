# 2026-10-06 — Any sub-delim but `+` may precede a clock

**Fix** — second review round on #276. The punctuation-clock branch of `redact.UsernameIsClock`
accepted only `,` `(` `*` `'`, so `Session;9:30@main.stage` was refused on the X screen and
blanked in `config_snapshot`. It now accepts every RFC 3986 sub-delim except `+` before a real
clock (hour <= 23, two-digit minutes <= 59). A leading sub-delim before a long numeric pair
(`!2024:1234@ops.example`, whose match starts at the digit) is caught by the two-digit bound on
the all-digit branch, with regression cases on both sides. The X package's concept no longer
describes the superseded digits-both-sides rule. See [pkg/redact](../code/pkg-redact.md).

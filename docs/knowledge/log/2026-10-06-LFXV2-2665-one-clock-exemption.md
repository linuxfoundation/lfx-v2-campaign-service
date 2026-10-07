# 2026-10-06 — One clock exemption, bounded digit pairs

**Fix** — review of #276. The all-digit branch of the userinfo clock exemption had no length
limit, so a numeric user ID and PIN (`2024:1234@ops.example`, `12345:67890@host.example`) was
still exempt from `config_snapshot` redaction and the X tweet screen. That branch now allows at
most two digits a side (`14:00`, `9:30`, and the score `3:4` stay ordinary copy). The function
was duplicated byte for byte in `internal/platform/twitter`; it is now exported once as
`redact.UsernameIsClock` and the X screen calls it, so the two cannot drift. Edge tests pin hour
24, minute 60, a one-digit minute after punctuation, `Mon,23:59`, and the long numeric pairs on
both sides. See [pkg/redact](../code/pkg-redact.md) and
[internal/platform/twitter](../code/internal-platform-twitter.md).

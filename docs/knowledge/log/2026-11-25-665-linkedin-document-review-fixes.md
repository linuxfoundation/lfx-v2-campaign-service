# 2026-11-25 — #665 document ads: PR #683 review fixes

**Fix** — A LinkedIn document create no longer requires the brief's registration URL: the client
skips that check for `format: "document"`, whose post carries the PDF and no landing page (every
other format still requires it). The PDF upload check now requires the file to END with its final
`%%EOF` (followed only by CR, LF, space, tab or NUL, within the last 1024 bytes), so an incremental
update cut off after an earlier revision's marker is refused instead of stored as complete.

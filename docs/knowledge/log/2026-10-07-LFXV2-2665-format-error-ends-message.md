# 2026-10-07 — A format error ends the sanitized validation message

**Fix** — review of #282. Goa's format validators append their parse error to the quoted value
UNQUOTED (FormatUUID: `uuid: <value>: <cause>`), so a value carrying `; "X" is missing from path`
forged a part that `sanitizeValidationPart` kept, echoing caller text. `sanitizeValidationMessage`
now treats a format part as the end of the message: it is rewritten, and if anything followed it,
one generic sentence stands in for all of it. Pinned by a `goa.ValidateFormat(..., FormatUUID)`
unit case and by `TestUUIDPathRejection_DoesNotEchoForgedParts` on the real get-brief route. See
[cmd/campaign-service](../code/cmd-campaign-service.md).

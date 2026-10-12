# 2026-10-07 — LFXV2-2665 Meta: trust an error envelope only if it decodes cleanly

**Fix** — `do()` discarded the `json.Unmarshal` error on a non-2xx Graph envelope, so a body such
as `{"error":{"code":100,"is_transient":"true"}}` — decoded to `Code=100`, `IsTransient=false`
plus a type error — could be marked trusted and classified REJECTED ("nothing was changed") by
`ClassifyAdSetWrite` instead of UNCONFIRMED (#288 review). An envelope is now trusted
(`APIError.EnvelopeParsed`) only when the schema decode returns no error AND `identityjson.Check`
accepts the raw body; the partially decoded fields are still copied for throttle detection and
logging exactly as before. New classification rows: `is_transient` as a string and a non-string
`message` (both caught only by this fix), `code` as a string and `error` as a non-object (also
UNCONFIRMED), and extra unknown fields (still REJECTED — the decoder does not disallow them).

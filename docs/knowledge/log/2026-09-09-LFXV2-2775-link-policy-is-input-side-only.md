# 2026-09-09 — LFXV2-2775: the link policy is enforced on the input side only

**Note** — the shared prompt tells the model that every `href` in the generated body must be the
supplied Registration URL copied exactly, and that with no URL the call to action is plain text
with no `<a>` tag. Nothing enforces that on the way back out:
`parseEmailCopyResponse` (`internal/service/email_copy.go`) validates JSON shape and
`maxBodyRunes` and nothing else, so a model that emits `href="#"` or an invented address produces
a response the service accepts.

What IS enforced is the input side — `httpURL` now validates and escapes the URL before it reaches
the prompt, so the worst an obedient model can copy is a safe value. The two are mitigations for
different failures: the instruction addresses the model choosing a wrong link, an output check
addresses the model ignoring the instruction.

The output check was deliberately deferred rather than added alongside the URL fix. It is not a
validation tweak: it needs a decision about what happens when a model returns a bad href — reject
the generation, strip the tag, or pass it through with the anchor neutralised — and each is a
behaviour change visible to the operator. It belongs with the `maxBodyRunes` guard, where the
"model response is unusable" path already exists, and on a change scoped to that decision.

Raised by review on #207; recorded here so the gap is not mistaken for enforcement by a later
reader of the prompt text.

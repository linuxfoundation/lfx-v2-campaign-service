# 2026-08-24 — LFXV2-3295 the /adimages part name was documented but not pinned

**Fix** — the concept file claimed `/adimages` is called "with the correct parameter", implying
this client uses Meta's documented `bytes` parameter. It does not, and no test would have caught
a change to what it does use.

`uploadImage` sends a raw multipart file part named `source`. Meta's `POST /act_<id>/adimages`
reference documents two create parameters — `bytes` (a Base64 UTF-8 SCALAR) and `copy_from` — and
documents no multipart file field at all. So the doc sentence described a transport the code does
not take, on an endpoint where the published reference actively points a reader the wrong way.

The prior day's fragment had already derived the right answer from Meta's official SDKs (Python
uploads under `source0`, PHP under a filename param) and recorded that the docs are SILENT on the
multipart mechanism rather than contradicting it. The concept file simply had not been brought in
line, so the bundle asserted two different things about the same call.

**The gap that made this more than a wording defect:** the part name was pinned by nothing. The
upload test asserted only that the field name was NON-EMPTY, so renaming the part to `bytes` —
the exact move the reference invites, and the one two reviewers on this PR independently argued
for — compiled, passed the whole suite, and would have broken every creative upload against a
real account.

The assertion now excludes `bytes` specifically, with the reason attached: a raw file part under
the base64 scalar's name is neither the documented transport nor the SDK transport. It
deliberately does NOT assert `source` as the only acceptable name, because Meta's own two SDKs
disagree on it and pinning one literal would encode a choice Meta has not published. Excluding
the known-wrong name is the claim the evidence supports.

Mutation-verified: renaming the part to `bytes` fails the test with that message.

Also corrected in this sweep: `**Change**` is not in CLAUDE.md's knowledge-log marker vocabulary
(`Update`, `Fix`, `Creation`, `Note`, `Verification`, `Docs`) and appeared in no other fragment;
and the 2026-08-21 fragment carried no marker at all.

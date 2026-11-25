# 2026-11-25 — #664 LinkedIn lead gen pre-PR review fixes

**Fix** — The lead gen form read treats only a 404 as "not this account's form"; a 403 (which
can equally be a token or role that cannot read forms) now fails the create as an upstream error
rather than a misleading pre-flight refusal, nothing created either way. The form read moved
above the #584 group-lock comment, whose wait-abort message now says "before any LinkedIn write".
The #582 objective docs (contract text and api-catalog table and readback note) list
`lead_generation`.

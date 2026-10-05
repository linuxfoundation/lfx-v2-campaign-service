# 2026-10-05 — Microsoft config_snapshot redacts links inside keywords

**Fix** — Follow-up to the Microsoft `config_snapshot` scrub (#256). That change kept
`keywords[].text` verbatim to match Google's policy, but a keyword is caller text that reaches
the UNENCRYPTED snapshot: `validateKeywords` only trims, length-checks and validates the match
type, so `https://example.test/reset/SECRET?token=VALUE` is a valid keyword and was stored as
written — against the knowledge-base rule that caller URLs are redacted before
`config_snapshot` (copilot review on #256).

New `sanitizeSnapshotKeyword` (`internal/dispatch/creds.go`) is `sanitizeSnapshotText` minus its
path-only pass: it redacts scheme-ful links, scheme-less links with a query or fragment, and
scheme-less `user:pw@host` runs (dropped whole, as the shared userinfo pass does), and keeps a
scheme-less `host.tld/path` with nothing after it — for a keyword that is targeting text
(`k8s.io/docs tutorial`, `node.js/express`, `10.0.0.0/8`), not a secret. `microsoftSnapshotConfig`
reallocates the keyword slice and applies it to each keyword; the config sent to Microsoft is
unchanged. Pinned by `TestMicrosoftSnapshotConfig_KeywordLinksRedactedTargetingTermsKept`.

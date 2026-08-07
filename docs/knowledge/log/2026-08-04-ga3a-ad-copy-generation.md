# 2026-08-04 — GA-3a: ad copy generation

**Update** — Added GA-3a ad-copy generation (`internal/platform/googleads/ad_copy.go`, split out
of GA-3's ad-group/ad cascade to keep the PR under 1000 lines) and fixed a credentials-leak in its
`buildAdFinalURL`: the caller-supplied registration URL was echoed unredacted into its validation
error messages, including via `%w`-wrapping a `*url.Error`. Added `redactURLForError` (mirroring
the twitter client) and applied it across all five error sites (parse failure, bad scheme, missing
host, embedded userinfo, malformed query); also added the userinfo-rejection
(twitter/reddit/meta/microsoft/linkedin) and malformed-query-rejection (reddit/meta/microsoft/
linkedin) checks those clients already carry.
New concept section in `internal-platform-googleads.md`.

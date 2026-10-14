# 2026-10-13 — LFXV2-2665 LinkedIn audience projection and cancellation

**Fix** — Review of #322 asked whether the LinkedIn audience read's `fields=…,pivotValues` is
rejected, as a comment in `internal/platform/linkedin/monitor.go` claimed for
"pivotValue/pivotValues". LinkedIn's ads-reporting page settles it the other way: it lists
`pivotValues` among the Analytics finder's supported `fields`, its `pivot=MEMBER_COMPANY` sample
projects it, its `MEMBER_COUNTRY_V2` and URN-resolution samples send `fields=impressions,pivotValues`,
and it says unprojected requests return only impressions and clicks. The projection therefore
stays; the monitor comment now confines the recorded 400 to the legacy singular `pivotValue`, and
the audience client documents the citation. Cancellation is now tested: with two campaigns (10
requests, 5 queued), one failing request cancels the four in flight, none of the queued five is
sent, and the read returns an error with no partial result. Queued tasks also check the context
explicitly before sending. Concept updated.

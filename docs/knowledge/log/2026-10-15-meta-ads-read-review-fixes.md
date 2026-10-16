# 2026-10-15 — D7 Meta ad read: pre-PR review fixes

**Fix** — `list-meta-ads` now answers its identity-mismatch 409 ("Meta reports an ad of this ad
set under a different ad account …") and unsupported-window 400 ("… for Meta ads") in its own
words instead of the ad-set read's, pinned in `TestListMetaAds_StatusMapping`; ad and creative
ids are checked through a named `validateGraphObjectID`; the
[Meta Ad Read Under an Ad Set](../architecture/meta-ads.md) concept says "chart comments only".

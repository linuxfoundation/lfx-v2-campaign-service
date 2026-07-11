---
type: "Go Package"
title: "internal/platform/twitter"
description: "X (Twitter) Ads v12 client: OAuth 1.0a signing and the campaign -> line_item -> promoted_tweet creation flow."
resource: "internal/platform/twitter"
---

# internal/platform/twitter

Package twitter is the X (Twitter) Ads API v12 platform client. It implements
OAuth 1.0a (HMAC-SHA1) request signing and drives the
campaign -> line_item -> promoted_tweet creation flow. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database.

`CreateCampaign` is idempotent: it reuses existing campaigns and line items by
name (paged cursor lookups via `findByName`) before creating new ones. A lookup
that fails transiently propagates an error so the caller aborts rather than
creating a duplicate. All entities are created with `entity_status=PAUSED`.

Per the X Ads v12 contract, create endpoints take their parameters as URL query
parameters (not a JSON body); the client folds those params into the OAuth
signature base string. Line-item creates include the required `start_time` and
`end_time` (ISO8601 UTC). Dates are validated for both shape and real-calendar
validity (`time.Parse`) before any mutating call. Writes honor the 1-req/sec
limit and retry 429s with backoff bounded by `Retry-After` / `X-Rate-Limit-Reset`.

See [internal/platform/twitter](../../../internal/platform/twitter).

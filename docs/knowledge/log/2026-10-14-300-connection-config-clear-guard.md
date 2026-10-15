# 2026-10-14 — Connection PUT refuses to clear a stored config field by omission (#300)

**Fix** — A connection `PUT` that left out an optional provider-config field wrote `""` over the
stored value. For Reddit that cleared `conversion_pixel_id`, which every Reddit campaign create
needs, so a label-only edit answered 200 and disabled campaign creation for the project.

- `updateConnGuarded` (`internal/service/connection_handler.go`) refuses, with 400 naming each
  field, an update that omits an optional config field whose stored value is non-empty. The check
  runs before any write and judges the row at the `If-Match` version (412 if it has moved; the
  version-conditioned write closes the race). An explicit `""` still clears, a sent value still
  replaces, and omitting a field with nothing stored is allowed. A body that sends every optional
  field performs no extra read.
- Opted in: Meta `app_id`; Reddit `conversion_pixel_id`; X `as_user_id`; Microsoft
  `customer_id`; HubSpot `portal_id`, `sender_email`, `sender_name`, `brand_kit`. LinkedIn has no
  optional config field (`org_id` is Required). Google Ads (`login_customer_id`) is not changed;
  opting it in is one line.
- Whenever the handler reads the row (for this guard or the force-system guard), a stale
  `If-Match` is answered 412 before either guard's 400.
- X's `as_user_id` Pattern is now `^([0-9]+)?$`, so an explicit `""` can clear a declared
  identity.
- The rule is in each guarded attribute's design description (`keepOnUpdate`), so it is in the
  generated OpenAPI.
- `TestRedditAdsConversionPixelRoundTripAndClear` and the renamed
  `TestUpdateTwitterAds_OmittedAsUserIDIsRefused` now pin the refusal; new
  `TestUpdateConnection_OmittedConfigKeyGuard` covers every guarded key. `TestClearGuardProviders_CoverEveryConfigKey` fails when a provider gains a config key the
  table does not classify, `TestUpdateConnection_ConcurrentWriteAfterTheGuardReadIs412` pins the
  race argument, and `TestForceSystem_StaleIfMatchIs412BeforeTheAccountGuard` pins the ordering.

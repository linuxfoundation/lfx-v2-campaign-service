# 2026-11-06 — User-list account_id is limited to the create's account on the LF credential

**Fix** — on the LF system credential, `list-google-ads-user-lists` refuses an `account_id` other
than the account a create uses, with a 400 before Google is called (#512, a follow-up to #508).
Without this, the route, which #508 opened to `global_marketing_ops` on the shared account, could
list user lists on every customer the LF login reaches, for example other customers under a
manager account. The picker needs only the create's account; the conversion-action and audience
reads take no `account_id` at all.

- The refusal is `domain.ErrAccountIDNotAllowed`, kept apart from `ErrAccountIDMalformed`, so a
  policy refusal is not counted or worded as malformed input. Its 400 says to omit `account_id`.
- It applies on the fallback and under the forced LF account (`resolved.isFromSystem`). A
  project's own credential is unchanged.
- `ListUserLists` also returns `called`, so local refusals are not recorded as Google calls (#508
  review).

See [google-ads-targeting-lookups](../architecture/google-ads-targeting-lookups.md).

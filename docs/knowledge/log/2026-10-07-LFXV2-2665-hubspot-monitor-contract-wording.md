# 2026-10-07 — LFXV2-2665 HubSpot monitor contract wording

**Fix** — Four more PR #290 review threads on `monitor-hubspot-account`.

- **Remedy text.** The unusable-connection remedy named the persisted `privateAppToken`, not the
  wire field `private_app_token` a caller can send, and only asked for the field to be set — no
  help for the 401/403 path, where the token is present but revoked or missing scopes. It now
  names `private_app_token` and asks for a valid token with the marketing-email scopes. The
  service classification test asserts both.
- **`AsOf`.** The model field and the `internal-platform-hubspot` concept still described a single
  read instant; both now say it is the client clock at the last response, an upper bound.
- **Status codes.** The `internal-service` concept now says a HubSpot 401/403 is the 400/500
  unusable-connection case, not part of the upstream 503.

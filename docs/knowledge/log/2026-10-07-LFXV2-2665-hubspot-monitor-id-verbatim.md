# 2026-10-07 — LFXV2-2665 HubSpot monitor validates stored ids verbatim

**Fix** — Two PR #290 review threads on `monitor-hubspot-account`.

- **Verbatim ids.** `hubspotMonitorTargets` trimmed whitespace before `ValidateEmailID`, so a
  malformed stored id such as `" 123 "` was requested rather than counted, and a present A/B
  variant with an empty or blank id was silently omitted. Ids are now taken verbatim: the
  validator rejects a padded id and it is counted unattributable, and a PRESENT variant is always
  a candidate (empty/blank → counted); only an absent (or null) variant contributes nothing.
- **Status wording.** An unusable connection is 400 only when it is the project's own; the LF
  system fallback's is 500 (`ErrSystemConnectionNotUsable`, operator-owned). The handler comment,
  catalog row, architecture concept and internal-service concept now say so, and the service
  classification test pins the 500 case.

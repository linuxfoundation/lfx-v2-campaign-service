# 2026-11-06 — #511 follow-ups: stored account id quoting, OperatorMessage comment, KB nits

**Fix** — Leftovers from #495 and #493.

- The Google Ads foreign-account refusals quote the connection's own stored account id with
  `%q`, as they already quoted the caller's owner id. `validateAccountIDs` checks digits, not
  length, so a mis-stored 40-digit id reached the job result unwithheld. Eight call sites:
  conversion action, Demand Gen Audience, Demand Gen demographics user list and custom
  audience, Display audience resource, Performance Max audience signal, shared negative keyword
  list, Search audience segment. A new sweep,
  `TestPreflightCampaignKind_StoredAccountIDIsQuotedInForeignAccountRefusals`, runs against a
  40-digit stored id and fails if any operator message shows it, or if its channel's sweep
  never reaches one of the eight sites. Reverting any one of the eight
  sites to `%s` fails it. See `code/internal-platform-googleads.md` § Pre-flight refusals reach
  the job result (#494).
- `PreflightRefusal.OperatorMessage`'s doc comment no longer says it is `Error()` under another
  name. For `WrapQuotedPreflightRefusal` it returns `quotedCallerValues(err.Error())`, which
  differs from `Error()`.
- Correction to `2026-11-05-492-image-asset-helpers.md`, left as written because entries are
  not edited after the fact: that entry says the Search assets test checks only the
  business-name text asset's create for a `name` key. Since #493's second commit it checks
  operations 3–7 for one: the business-name text asset and the four campaign asset links.
- `code/internal-platform-googleads.md` § Image asset names: a ~135-character line re-wrapped
  to the ~95 around it.

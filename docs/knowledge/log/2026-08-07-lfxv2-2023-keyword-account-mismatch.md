# 2026-08-07 — LFXV2-2023: the keyword plan's dispatchers skipped the creation-account guard

**Update** — `ReadMetrics` and `ToggleStatus` both compare `googleAdsCreationCustomerID(campaign)`
against `client.CustomerID()` before contacting Google (`internal/dispatch/googleads.go:340`,
`:417`). The keyword plan's `ListKeywords` and `UpdateKeywords` resolved a client and went
straight to the call.

**Fix** — The guard is not defensive padding. `PlatformCampaignID` is a bare numeric, unique only
within the customer it was created under, while the connection just resolved is the project's
CURRENT one — `UpdateGoogleAds` can re-point it between create and read. A keyword query scoped to
that id under the wrong customer **does not error**: it returns no rows, which is
indistinguishable from a campaign that genuinely has no keywords. On an id collision it returns
another account's keywords and spend.

**Fix** — It matters more on the write path, and `AuthorizeKeywordCriteria` is not a substitute:
it proves each criterion lives under this campaign id, but the campaign id is itself only
meaningful within a customer. Under the wrong customer the authorize either finds nothing (every
action fails, merely confusing) or, on a collision, authorizes and then PAUSES or REMOVES another
account's criteria.

**Fix** — Both handlers map `ErrCampaignAccountMismatch` to a **scrubbed 409**, copied from
`GetCampaignMetrics` (`internal/service/brief.go:533-547`) and `ToggleCampaignStatus` (`:700-709`).
Scrubbed twice over: the response omits both customer ids because which account a project is
connected to is connection configuration, and the LOG goes through `safeErrSummary` because the id
comes from the connection's `account_id` — a design attribute with no Pattern, MaxLength, or
charset constraint — and this guard runs before any request, so the client's own
`validateAccountIDs` has not executed on the instance yet. Two neighbouring log lines in the plan
still passed the raw error; they now scrub as well.

**Fix** — The mismatch tests assert **zero requests reached the server**, not just the returned
error. An error-only assertion passes against a guard placed after the platform call — which on
the update path means the criteria were already mutated in the wrong account before the error was
produced, and on the read path means there is no error to assert on at all.

**Note** — Copilot's finding closed with "the later claim that no `CustomerID()` accessor exists is
incorrect", and it was right: the accessor landed on `main` with the metrics read. Two paragraphs
of the plan argued a client-package boundary from an absence that no longer holds. Both now say
what is actually true — the URL is built in the client because URL construction belongs in one
package, and the accessor's purpose is this one comparison.

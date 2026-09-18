# 2026-09-18 Email content: privacy leak, destructive rebuild, and verification gaps

**Fix** — six issues found reviewing the email-content branch, the first blocking.

**Cross-tenant read (privacy).** `BuildReferenceBlock` searched the HubSpot portal
with an empty needle, which matches every email the portal returns, while
`projectID` only chose the *connection* and never filtered results. On the shared
`model.SystemProjectID` connection that seeded one project's generated copy with
another project's past sends — attendee counts, sponsor names, pricing,
unpublished agenda detail. Under one shared LF connection that is every project
without its own, not an edge case. `resolveClient` now reports `fromSystem` and
the block is skipped when it fires. This is unlike `internal/dispatch`'s fallback,
which is legitimate: that path only ever *writes* the requesting project's own
content to the shared portal. Re-enabling needs a per-project ownership signal on
the emails to filter by; there is none today.

**A rebuild that could not preserve the body.** The no-content guard in
`applyEmailContentWithHero` was widened to treat `previewText`/`sentByOrg` as
content, so a preheader-only change would apply. That was wrong and was reverted:
`RebuildEmailContent` replaces the whole widget tree by design (the wipe is what
stops the clone source's stale content leaking), so a rebuild carrying no body
either writes an empty `staging_body` over the clone's body or omits the section
and drops it from the tree. Both are data loss. Preview text is only settable
through the content tree — the Marketing Emails v3 object has no preheader field
— so a preheader-only change genuinely cannot be applied today. The gap is now
pinned by `TestHubSpot_APreheaderOnlyConfigLeavesTheDraftAlone` so the next person
finds the reason before re-widening the guard.

**Verification accepted a partial apply.** `verifyContentSaved` returned on the
first matching widget, so one surviving key from an earlier generation confirmed a
rebuild whose new body and layout were lost. It now requires every widget it wrote
and names the missing ones. Its read failures are also separated: a timed-out or
malformed verification GET is `errVerifyReadFailed` (unknown, retryable) rather
than `ErrContentNotPersisted` (proven revert, which the dispatcher logs at ERROR
as needing manual repair).

**Three smaller ones.** Two `downloadImage` error paths (`url.Parse`,
`NewRequestWithContext`) rendered the complete input, leaking a signed hero URL's
query into error text and logs. The upload response read `maxResponseBody+1` to
detect truncation and never acted on it, so a truncated body could confirm a
non-idempotent create. Model-produced button URLs were unbounded against
`design/brief.go`'s `MaxLength(2000)`, failing Goa as a 500 naming nothing rather
than the 503 they are — rejected rather than truncated, since a cut URL is a live
link to the wrong place.

**Docs.** `docs/api-catalog.md`, the `internal/service/email_copy` concept doc and
its index bullet described the pre-`sections` response shape; the concept doc also
described the reference lookup as using the shared portal, which the privacy fix
forbids.

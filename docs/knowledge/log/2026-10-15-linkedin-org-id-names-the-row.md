# 2026-10-15 — LinkedIn's missing-org-id refusal names the row it read

**Fix** — `LinkedInDispatcher.Dispatch`'s missing-org-id refusal now names the LF system
connection when that is the row it read.

- The org id is checked inline after `resolveLinkedInCredentials`, outside `systemScoped`, so
  the refusal always said `linkedin connection for project "<id>" is missing org id`, even when
  the credentials came from the LF system fallback row (or the forced-system row).
- `linkedInMissingOrgIDRefusal` picks the wording from `res.isFromSystem()` with issue #208's
  connection-class sentence heads: `this project's linkedin-ads connection is missing its org
  id; nothing was sent` or `the LF system linkedin-ads connection is missing its org id; nothing
  was sent`. It is still a `PreflightRefusal` wrapped in `notCreated()`, so the claim is
  released and nothing is sent.
- Tests: `TestDispatch_LinkedInMissingOrgIDNamesTheRowItRead` (project row, unforced fallback,
  forced system row over a complete project row; zero requests). Mutation-checked: disabling
  the provenance branch fails both system cases, and dropping `notCreated()` fails all three.
- Concept updated: `code/internal-dispatch.md`.

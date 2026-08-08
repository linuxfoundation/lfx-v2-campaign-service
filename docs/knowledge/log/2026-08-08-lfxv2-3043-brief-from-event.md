# 2026-08-08 — Bridging event details to persisted briefs

**New** — `BriefFromEventDetails` in `internal/service`, mapping parsed event metadata
(`internal/platform/eventurl.EventDetails`) into a `model.CampaignBrief` ready for persistence
via the existing `BriefService.CreateBrief` path. Closes the event-URL → brief generation
pipeline (LFXV2-3043).

## Only confidence-high fields are mapped

The parser extracts with three strategies in precedence (JSON-LD, OpenGraph, fallback HTML),
but the *mapper* is intentionally conservative:

- **Extracted fields that flow through** — Name, Description, Location, StartDate, EndDate,
  Image, URL — are stored in the brief's `EventDetails` blob verbatim. These come directly
  from the page; no interpretation needed.
- **Human-judgment fields are left empty** — Copy (ad copy), Keywords, Targeting
  (targeting recommendations), and Platforms (binding platform selection) — because the
  creator is the only one who knows the campaign's audience and objective. Leaving these
  empty for manual authoring is correct; inventing plausible values and then dispatching
  them to paid ad platforms is not.

A wrong guess becomes ad copy that spends money; an empty field is honest. The brief starts
draft, awaiting human authorship.

## EventName is required — a defensive guard

The parser checks for Name and returns no result if it is empty. The mapper re-checks it as
a precondition: a missing Name is an error, returned clearly at mapping time rather than
silently producing a brief that will fail dispatch two steps later when the dispatcher reads
the blob and finds no `eventName` key. A page with no usable title is a client error
("this page has no event metadata"), not a permission failure or an upstream outage.

## The mapping reuses existing persistence invariants

Rather than inventing a parallel brief-creation path, the function returns a `model.CampaignBrief`
that is passed directly to `BriefService.CreateBrief`, which handles:

- ID generation and version initialization
- Status setting to `draft`
- Optimistic-concurrency persistence with If-Match guards
- Outbox enqueuing for index publication (Query Service)
- Stamping CreatedAt/UpdatedAt

A route handler that wants to expose this mapping calls the function, then the service method,
in sequence.

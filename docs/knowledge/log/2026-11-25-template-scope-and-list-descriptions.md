# 2026-11-25 — Brand-scoped template search and plain-English list descriptions

**Update** — two read-only additions for the event email flow, so an operator sees only the
templates that belong to the event's brand and can read what a selected audience means.

- `GET /projects/{projectId}/connection-hubspot/emails` takes `match`: up to 8 phrases, and an
  email is returned when ANY of them appears in its name or subject as whole words
  (`hubspot.Client.SearchEmailsAnyPhrase`, one walk for every phrase). The template picker sends
  the event's brand phrases ("KubeCon", "CloudNativeCon") instead of listing the portal's newest
  500 emails, which on the LF portal span every brand and a little over a week. `match` and `q` are
  mutually exclusive. Both searches now have their own 40s deadline (`emailSearchCallTimeout`):
  a full filtered walk measured 15-16s, and the shared 20s ceiling failed a live search.
- `POST /projects/{projectId}/audience-builder/lists/describe` renders each list's HubSpot filter
  tree into plain-English rule groups (`audience.DescribeRules`, deterministic, no model), naming
  referenced lists, properties, event types and email subscriptions within a lookup budget
  (`AudienceExplorer.DescribeLists`). Static lists, missing lists and unreadable lists are
  reported as such without failing the batch. Routed in the HTTPRoute regex and the RuleSet, with
  parity rows.

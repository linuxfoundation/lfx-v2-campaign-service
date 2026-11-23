# 2026-11-23 — Survey emails are rebuilt in the survey layout

**Update** — A staged survey draft was a full rebuild in the EVENT chrome: no LF Research logo and
no View-in-browser strip, a "FOLLOW US" header with black circle icons and the LFX Insights mark,
and "This email was sent by: The Linux Foundation Events". Every sent LF Research survey email
(State of Neocloud, Virtual Machines, CNCF Annual 2026, FINOS 2026) carries one different, shared
chrome. The draft was compared through its `/draft` content, because the live view of a cloned
draft still shows the clone source's content and made the two look alike.

- **`hubspot.EmailLayout`.** `RebuildEmailContentInput.Layout`, zero value `LayoutEvent` so every
  existing caller is unchanged. `LayoutSurvey` writes the sent email's order: View-in-browser strip,
  logo band, hero, body, button, navy divider, colour social row, divider, sent-by line and native
  footer. No sponsor tiers, no "FOLLOW US". The sent-by organisation defaults to "The Linux
  Foundation" and is HTML-escaped, which the event layout's footer line is not.
- **Dispatcher.** `emailLayoutFor` picks the layout from the brief's delivery type and passes it to
  the clone and its A/B variant.
- **`surveyCopyWarnings`.** Advisory: a percentage, calendar date or question or minute count in the
  generated body that the brief's source text does not contain becomes a dispatch warning, the
  research team's "never invent" rule checked rather than only asked for in the prompt. The source
  is read with the same patterns as the body and compared as claim keys, not raw text: a figure
  counts as stated only whole ("5%" is not backed by "35%", "8.5 minutes" is not "5 minutes"), a
  hyphen or en dash between numbers is a range checked with both ends ("3-5%" does not back "35%",
  "10-15 minutes" does not back "5-15 minutes"), the unit is compared in the singular ("a 3-minute
  questionnaire" backs "3 minutes"), and Variant B's body is checked with A's.
- **`nativeFooterWidget`.** The native footer is built in one place for both layouts, pinned by
  `TestRebuildEmailContent_EventLayoutIsUnchanged`.

Not done, on purpose: the legal block (privacy, visibility, questions) and the incentive stay inside
the generated body as before; the sent emails' `/email_footer no links` custom module and their
`#0094ff` button are not copied (see `internal-platform-hubspot.md`); the hero image still has to be
supplied by the caller, as nothing on the survey surface collects one; sender name, reply-to and
subscription type are still inherited from the clone source.

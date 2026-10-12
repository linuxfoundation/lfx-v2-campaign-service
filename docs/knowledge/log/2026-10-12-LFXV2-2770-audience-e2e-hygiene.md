# 2026-10-12 — Audience builder fixes from the end-to-end UI test

**Fix** — found by walking the Audience tab on the `tlf` portal (AgntCon Japan):

- **Suppression lists were offered for INCLUSION.** Discovery classified opt-out, GDPR and master
  exclusion lists as `uncertain` and returned them as include candidates. `DiscoveryHidden` now
  keeps suppressions, master lists, test lists and staff lists (and unclassified file uploads) out
  of the discovered lists entirely; they are counted in the new `hidden` field. Suppressions are
  still offered by `suppression-lists`, masters by `existing-master-lists`.
- **No brand.** When the event page names none, discovery and event-audience interpretation take
  the brand the event's own lists agree on (`DominantBrand`, "<YYQN> - <BRAND> - …"; needs two
  agreeing names and no tie).
- **Reuse suggestions named other events "likely the same audience".** `GuardReuse` downgrades a
  suggestion whose name shares no event word with the label and is not a general audience list.
- **Questions never settled.** `SettleQuestions` drops answered questions and ends the loop after
  three answers; the prompt asks for at most one further question.
- **Event audiences now include this event's past editions**, resolved from the warehouse
  (`AudienceBuilder.ResolvePastEditions`) and added as `registered_for` verbatim.
- **Recent sends explain themselves:** each row carries `subject` and `match_reason`
  (`event_name` | `subject` | `related_words` | `brand_only`, `LastSentMatchReason`).
- Interpretation returns the `event` it planned from, so the UI can run QA's current-registrants
  check outside discovery.

**Fix, same day, found by the re-test** — `DominantBrand` read the event's OWN name back as its brand
("AGNTCon + MCPCon Japan"), because some of the event's lists file it in the brand slot
("26Q3 - AGNTCon + MCPCon Japan - APAC Registrants"). It now takes the event name and skips any
segment sharing a word with it.

**Fix, same day** — QA's current-registrants check judged only the lists an audited list REFERENCES,
so this edition's own registrants list, used directly as the send list, passed. `RunQA` now counts
the audited list's own name as an inclusion for that check
(`TestRunQA_FlagsTheEventsOwnRegistrantsAuditedDirectly`).

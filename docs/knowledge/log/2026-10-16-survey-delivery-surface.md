# 2026-10-16 — A survey/report promotion delivery surface

**Update** — campaigns could be authored for two surfaces, `paid-marketing` and `email`. Survey and
report promotion had no home: it lived as a standalone FastAPI prototype
(`LF-Marketing-Ops/marketing/emailcreationskill/backend/survey_promo`) with its own HubSpot token,
its own HTML renderer and no audience compliance.

- `survey` is now a third `delivery_type`. Migration `000047` widens both CHECKs from `000030` —
  the delivery-type list and the delivery/stage pair rule — and is expand-only, so it is safe in a
  single release.
- `emailstage` gains a SECOND taxonomy: `SurveyNames()`/`SurveyTemplates`/`ResolveSurvey` for the
  `Survey Promo` and `Report Promo` stages, kept apart from the event stages so a cross-surface
  pair cannot resolve. `IsSurveyStage` is what lets `generate-email-copy`, which is given a stage
  but never a delivery type, pick the right map.
- The prompts are ported from the prototype and reproduce the LF Research specification verbatim,
  including the 100-word body cap, the anti-invention rule, the survey privacy/visibility/questions
  disclosure and the report's click-to-share line.
- A survey brief's source document rides in `event_details.sourceText` and is rendered into the
  prompt as `Source document`; its promoted URL is the brief's own `url`, so the existing
  call-to-action path is unchanged. No new column.
- Deliberately NOT ported: the prototype's hard-coded workflow and email ids, its fail-open
  suppression lookup and its single global HubSpot token. Audience, suppression, staging and the
  async job are the existing email ones.

Still open: the prototype's 3-email invite/reminder/deadline SEQUENCE, which needs a HubSpot
automation client this service does not have.

See [internal/service](../code/internal-service.md).

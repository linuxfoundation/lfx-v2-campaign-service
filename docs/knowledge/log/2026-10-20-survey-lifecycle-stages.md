# 2026-10-20 — The survey surface gets a reminder and a closing send

**Update** — 000047 gave the survey surface one stage per asset, `Survey Promo` and `Report Promo`.
That named WHICH ASSET is promoted but not WHICH SEND, so a survey promoted twice — an opening
email and then a reminder — had nowhere to put the second: both would have keyed to `Survey Promo`
and the later brief would have displaced the first, taking its audience and draft link with it.

- `Survey Reminder` and `Survey Closing` join the survey stage vocabulary, widened into the pair
  CHECK by migration `000048` (expand-only, so it is safe in a single release). Each is its own
  brief, generated, audienced and staged on its own, exactly as the six event stages are.
- Report promotion deliberately gets no counterpart: a published report does not close, so a
  closing send would have to name a deadline the source cannot state, and the anti-invention rule
  the survey prompts carry would have nothing to ground it in.
- The three survey sends share ONE prompt specification. `surveyParticipationHead` and
  `surveyParticipationTail` hold everything below the opening — who is fielding it, the goals, the
  topics, the impact sentence, the incentive, the CTA and the disclosure — and
  `surveyParticipationPrompt` assembles each stage from that plus its own opening. Three pasted
  copies would have drifted apart at the first edit. This mirrors the prototype's sequence design,
  where only the subject, headline and opening line vary across the series.
- The reminder's opening is told NOT to imply the reader has not yet responded: the send list is
  not filtered on who has answered, so that claim is wrong for everyone who has. The closing's
  opening may name a date only if the source states one, and is told explicitly that a date the
  source gives for something else — a coupon's expiry, a publication window — is not the survey's
  closing date.

Still open: the prototype's single-call sequence generation (one Generate writing the whole series)
and its HubSpot workflow automation, which needs an automation API client this service does not
have.

See [internal/service](../code/internal-service.md).

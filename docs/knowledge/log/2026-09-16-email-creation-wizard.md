# 2026-09-16 — email-creation wizard on the briefs service

**Creation** — eight new Goa methods on `briefs` walk an operator from a campaign brief to an
addressed HubSpot email draft, backed by a version-gated `wizard_sessions` row (migration
`000033`) and a hand-written server-sent-events progress route mounted outside Goa. See
[Email creation wizard](../architecture/email-wizard.md) for the shape.

The wizard existed as a separate prototype service with its own model, its own audience
plumbing and its own event-detail scraping. None of that came across. The endpoints are new Go
written against this repo's existing pieces — `emailstage.Resolve` for the stage guide,
`internal/utm` for link tagging, `eventurl` for the event's facts, `domain.AudienceRepository`
and `model.CampaignAudience` for the recipients, the existing `hubspot.Client` for the draft —
because a parallel audience model was the part most likely to disagree with the one dispatch
already sends against. The prototype's JSON field names ARE preserved exactly: an existing
frontend depends on them, and the wire shape is the one thing a rewrite must not change.

Three decisions that cost the most to get right:

**The turns are separate requests, and the session is the only state.** Each turn is a decision
an operator makes, and the clone turn creates something outside this service. So the phase, the
plan, both variants, the edited sections, the transcript and the created draft's id all live on
a Postgres row rather than in memory: a turn may land on any replica, and a reload resumes.
Writes are version-gated and `domain.ErrStaleWizardSession` maps to **409**, never 404 — the row
is what says whether a draft already exists, and a caller told "not found" for a stale write
starts a second session and clones a second draft. The same reasoning drives the one narrow
post-clone path: a draft created but a session save that fails answers 409 *naming the created
draft id*, so a retry reconciles instead of duplicating.

**Guidance had to move onto the row.** `extra_context`, `email_type` and `is_transactional`
arrive at plan-start and plan; the model call happens in a later request that has no such
fields. The first cut read them from the generate payload, which meant the API accepted "write
for sponsors, not attendees" and dropped it before any prompt reached the model — a defect that
compiles, returns 200, and produces plausible copy for the wrong audience. They are now
persisted on the plan and read back at generation and chat time, and
`TestWizard_PlanCarriesOperatorGuidanceIntoGeneration` asserts the string reaches the prompt.

**Failure is deliberately asymmetric.** The reference variant is the endpoint's output, so an
unusable model response is 503; the stage variant is an alternative, so its failure is stored as
`mode: "failed"` and returned inside a 200 rather than discarding a primary that already cost a
model call. Planning survives an unresolvable HubSpot connection by falling back to the stage
guide, while cloning refuses. The send list refuses in every direction rather than substituting
— no audience, an unbuilt newest audience, a missing list id, unreadable suppression ids, or a
portal that cannot be proven — because sending to the wrong list is unrecoverable the moment a
human presses send in HubSpot. An unreadable portal identity is 503, mirroring
`assertAudiencePortal`'s fail-closed answer rather than guessing a match.

Two structural moves the layering forced. `applyEmailContent` was to be exported from
`internal/dispatch`, but that package's tests import `internal/service`, so `service → dispatch`
would have closed a cycle; it is now `hubspot.ApplyEmailContent` behind a narrow
`EmailContentWriter`. Credential resolution splits the same way: the concrete resolver stays in
`internal/dispatch` (`ResolveEmailClient`), the narrow `HubSpotWizardClient` and
`HubSpotClientResolver` interfaces are declared in `internal/service` where they are consumed,
and the adapter lives in `internal/container` — the only package already depending on both, and
the place that can guard a typed-nil `*hubspot.Client` from becoming a non-nil interface.

Wiring is bound from ONE statement inside `bindBriefLiveBackends`, and `SetWizardBackend` is on
the `briefBackendSetter` interface, so a cold-started pod cannot bind the brief repositories
while leaving the wizard silently dead. `TestWizard_UnwiredDegradesNotPanics` walks all eight
routes with nothing bound: every one answers the typed 503, none dereferences a nil.

The SSE route is the part with no generated safety net. It authenticates from the
`Authorization` header only — no `?access_token=`, even though `EventSource` cannot set headers,
because a credential in a query string is logged by every proxy in the path and the UI already
uses a fetch-based reader. The progress token addresses the stream but does not authorize it:
the session is resolved from the token and the path's project and brief must be its own, with a
mismatch answering 404 rather than 403 because confirming the token exists elsewhere is the
leak. Delivery is a non-blocking per-process hub — a wedged browser must not stall the request
goroutine doing the model call — plus a 3-second poll that reports phase *changes* only, which
is what makes the stream work when the next turn lands on another replica. The loop selects on
the server's base context, not only the request's, so a draining pod tells its clients to
reconnect instead of handing them a closed socket to interpret.

`eventurl.EventDetails` gained `Speakers`, `Sponsors` and `RegistrationURL`, read from the
JSON-LD tier only, where the values are declared rather than guessed. Speakers come from
`performer` and deliberately not `organizer`: the organizer is the host, and naming them as a
speaker in generated copy is a factual error a reader would catch.

`renderWizardSections` is the single renderer behind generation, editing and cloning, so the
three cannot drift and the operator's edits — not the model's first draft — are what the draft
receives. It needs no model, which is what keeps the editing half of the wizard working when the
AI proxy is unconfigured.

Not covered by a live-database test: the `WizardSessionRepo` has column-order, actor-stamping
and tenancy tests that read the SQL text, but no Postgres round-trip test. `wizard_sessions` has
many columns written positionally, which is exactly the shape where a mis-numbered placeholder
compiles and passes text-level tests — a follow-up should add the round-trip.

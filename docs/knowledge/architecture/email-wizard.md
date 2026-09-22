---
type: "Architecture Doc"
title: "Email creation wizard"
description: "How the briefs service plans, generates, edits, clones and addresses a HubSpot email draft across turns, with a Postgres-backed session and a hand-written SSE progress stream."
resource: "design/brief_wizard.go"
---

# Email creation wizard

Eight endpoints on the **briefs** service that walk an operator from a campaign brief to an
addressed HubSpot email draft, all under
`/projects/{project_id}/briefs/{brief_id}/wizard/...`:

| Turn | Route | What it does |
| --- | --- | --- |
| Plan start | `POST .../wizard/plan-start` | Creates the session, mints its progress token, records the operator's guidance |
| Plan | `POST .../wizard/plan` | Resolves the event's facts and looks for a past email to clone from |
| Generate | `POST .../wizard/content` | Two model-written variants: one from a reference email, one from the stage guide |
| Edit | `POST .../wizard/sections` | Re-renders the email from the operator's edited blocks — **no model call** |
| Clone | `POST .../wizard/clone` | Clones the source email in HubSpot and writes the approved copy into it |
| Send list | `POST .../wizard/send-list` | Points the draft at its recipients |
| Chat | `POST .../wizard/chat` | Answers questions about the run, with the transcript persisted |
| Read | `GET .../wizard/{session_id}` | The session's current state |
| Progress | `GET .../wizard/progress/{token}` | Server-sent events — **not a Goa method** |

The turns are separate requests because each one is a decision an operator makes, and because
the clone turn has an effect outside this service that must not be re-run by a retry.

## The session is the state

`wizard_sessions` (migration `000033`) holds the run: its phase
(`planning` → `content` → `cloned` → `complete`), the plan, both generated variants, the
operator's edited sections, the chat transcript, the created draft's id and URL, and the
progress token. Nothing lives in process memory, so a turn may land on any replica and a
browser reload resumes rather than restarts.

Writes are version-gated. A lost update on this row is not cosmetic: the row is what says
whether a HubSpot draft already exists for this run, so `domain.ErrStaleWizardSession` maps to
**409**, never 404 — a caller told "not found" for a stale write would start a second session
and, on the clone turn, create a second draft. The same reasoning covers the one narrow
post-clone failure: when the draft was created but the session save fails, the 409 names the
created draft's id so a retry reconciles instead of cloning again.

Every read is scoped to `(project_id, brief_id, session_id)`. A session carries unannounced
marketing copy, and a UUID is not an authorization model.

## Guidance crosses turns, so it lives on the row

`extra_context`, `email_type` and `is_transactional` are supplied at plan-start and plan, while
the model call happens in a **later** request. They are persisted on the plan and read back at
generation and chat time. Reading them from the generate payload instead — which has no such
fields — silently accepted "write for sponsors, not attendees" and dropped it before any prompt.

## Rendering is deterministic and shared

`renderWizardSections` is the single renderer behind generation, editing **and** cloning, so the
three cannot drift and the draft carries what the preview showed. Every block value is escaped
except a `rich_text` block's own HTML, which is markup by definition; hrefs and image sources go
through `httpURL`, so a `javascript:` or `data:` URL from a model or a scraped page cannot reach
an anchor. Links are UTM-tagged before the draft is written, not after.

That HTML is **not** passed through unfiltered. `sanitizeWizardHTML` runs it against an ALLOW-LIST
of formatting tags and attributes, using a real tokenizer rather than pattern matching — a regex
over tag names loses to `<scr<script>ipt>`, and this input is model-supplied and adversarial by
assumption. Element content survives even when its tag does not, because dropping `<span>` should
not delete the words inside it; `script`, `style`, `iframe`, `object` and `embed` are the
exception, since their content is the payload rather than copy.

It runs at THREE points, deliberately: where the sections are parsed, again before they are
persisted, and again in `renderWizardSections` on every render. The first two make the stored
invariant "sections are sanitized" hold on its own, so a future consumer reading the column
directly cannot receive unsanitised HTML by forgetting to re-sanitise. The render-time call is
defense-in-depth on top of that, not the primary control — it is what stands between the stored
value and the two sinks that execute or render it, and it is deliberately NOT redundant: removing
it would make every one of those sinks depend on the write path having been correct.

Because the renderer needs no model, the editing half of the wizard keeps working when the AI
proxy is unconfigured. An empty section list, or one that renders to nothing, is refused rather
than stored: it would produce a blank email, and the likely cause is a client bug.

## Deletion is real, and creation cannot race it

`ArchiveBrief` is a SOFT delete, and a session's `chat_history`, `plan_result` and generated
bodies are operator- and model-authored text with no TTL. `ScrubSessionsForBrief` clears all of
them on delete and bumps `version`, so an in-flight turn holding a pre-scrub snapshot fails stale
rather than writing the transcript back. A partial scrub would be worse than none, because it
looks done.

`CreateSession` takes `SELECT ... FOR UPDATE` on the brief row. The composite FK only requires
the brief to EXIST, and a soft delete leaves it there — so without the lock a session could be
created against a brief that was archived a moment earlier, under READ COMMITTED.

## Failure is asymmetric by design

- The **reference** variant is the endpoint's output: an unusable model response is 503.
- The **stage** variant is an alternative: its failure is stored as `mode: "failed"` and
  returned inside a 200, so a primary that already cost a model call is not thrown away.
- Planning survives an unresolvable HubSpot connection by falling back to the stage guide;
  **cloning** refuses, because HubSpot is where the draft has to exist.
- The send list refuses rather than substitutes — no audience, an unbuilt newest audience, a
  missing list id, unreadable suppression ids, or a portal that cannot be **proven** to match
  all stop before the draft is touched. Sending to the wrong list is unrecoverable once a human
  presses send in HubSpot. An unreadable portal identity is 503, fail-closed.

## Wiring

The wizard's collaborators are bound by one `SetWizardBackend` call from inside
`bindBriefLiveBackends`, and `SetWizardBackend` is part of the `briefBackendSetter` interface —
so a cold-started pod cannot bind the brief repositories while leaving the wizard dead. With no
database, and during the cold-start window, all eight routes answer a typed **503**; none of
them dereferences a nil collaborator.

Credential resolution keeps the import graph one-directional: the concrete resolver stays in
`internal/dispatch` (`ResolveEmailClient`), the narrow `HubSpotWizardClient` and
`HubSpotClientResolver` interfaces are declared in `internal/service` where they are consumed,
and the adapter lives in `internal/container` — the only package that already depends on both.
`hubspot.ApplyEmailContent` moved into `internal/platform/hubspot` behind a narrow
`EmailContentWriter` for the same reason: exporting it from `internal/dispatch` would have
closed a cycle through that package's tests.

## The progress stream is hand-written

`GET .../wizard/progress/{token}` is mounted on the mux directly, outside Goa, because Goa v3
has no streaming HTTP result here. It therefore re-does by hand everything the generated
handlers get for free:

- **Auth from the header only.** No `?access_token=` fallback, even though `EventSource`
  cannot set headers — a credential in a query string is logged by every proxy in the path.
  The UI reads the stream with a fetch-based reader instead.
- **The token addresses the stream but does not authorize it.** The session is resolved from the
  token and the path's project and brief must be that session's own; a mismatch is 404, not 403,
  because confirming the token exists elsewhere is itself the leak.
- **A per-process hub plus a 3-second DB poll.** `Publish` never blocks, so a wedged browser
  cannot stall the request goroutine doing the model call; the poll reports phase *changes*
  only and is what makes the stream work when the turn lands on another replica.
- **Shutdown awareness.** The loop selects on the server's base context, not just the request's,
  and sends a `reconnecting` frame so a draining pod's clients move to a healthy replica instead
  of reading a closed socket as a failed run.
- One turn per stream: a `Done` frame ends it, so a finished run does not hold a connection and
  a goroutine open indefinitely.

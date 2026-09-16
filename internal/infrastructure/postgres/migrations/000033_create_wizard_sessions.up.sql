-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- wizard_sessions: one run of the email-creation wizard over a brief. The wizard is a
-- MULTI-REQUEST conversation (plan-start -> plan -> generate-content -> update-sections ->
-- clone -> set-send-list -> chat), and every turn after the first needs what the previous
-- turns decided: which past email was chosen as the clone source, which two content
-- variants were generated, which HubSpot draft was created.
--
-- WHY A TABLE AND NOT A MAP. The service this capability comes from kept sessions in
-- process memory. That is not portable here: this service is Helm-deployed with a
-- replica count above one and no session affinity, so the pod that answers a caller's
-- `generate-content` is routinely NOT the pod that answered its `plan`. An in-memory store
-- would therefore lose the plan for a fraction of requests that scales with the replica
-- count, and lose ALL in-flight sessions on every rollout -- a caller mid-wizard would see
-- its session vanish with no error it could act on. The SSE progress stream depends on the
-- same durability: its cross-pod fallback (see internal/service/wizard_progress.go) is
-- literally "poll this row", which only works because the row exists everywhere.
--
-- hierarchy: Project -> Brief -> WizardSessions (a brief may be run through the wizard
-- more than once; each run is its own row, and nothing supersedes an earlier one).
CREATE TABLE IF NOT EXISTS wizard_sessions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- project_id is TEXT, not UUID, matching campaign_briefs.project_id (converted to TEXT
    -- in 000003) and every other project-scoped table here: project ids are slugs like
    -- 'cncf' as often as they are UUIDs, and a UUID column would reject the slug form.
    project_id      TEXT        NOT NULL,
    brief_id        UUID        NOT NULL,
    -- phase is the session's lifecycle position, mirroring the `wizardPhaseEnum` vocabulary
    -- in design/brief_wizard.go: planning -> content -> cloned -> complete. It is TEXT with
    -- a CHECK rather than a PostgreSQL ENUM for the same reason the other status columns in
    -- this schema are: adding a value to an ENUM type is a migration that cannot run inside
    -- a transaction with its users, while widening a CHECK is an ordinary one.
    phase           TEXT        NOT NULL DEFAULT 'planning'
                    CHECK (phase IN ('planning','content','cloned','complete')),
    -- progress_token addresses the SSE stream for this session. It is NULLABLE because a
    -- session is legitimately created without one (a caller that does not want progress
    -- frames simply never opens the stream), and it is not UNIQUE: a caller may reuse a
    -- token it already holds across a re-plan, and rejecting that would turn a harmless
    -- client retry into a 409 in the middle of a wizard run.
    progress_token  TEXT,
    -- The four JSONB payload columns are pass-through state: this service writes them from
    -- what the model produced and reads them back to answer later turns and the session
    -- endpoint. Nothing in SQL queries INTO them, so they are JSONB (not TEXT) purely for
    -- the type check on write -- a corrupt blob is refused at the INSERT rather than
    -- discovered by a decoder several turns later.
    plan_result       JSONB,
    reference_variant JSONB,
    stage_variant     JSONB,
    sections          JSONB,
    -- email_id / draft_url name the HubSpot draft this session created, once `clone` has
    -- run. They are a CACHE of what HubSpot owns, not the system of record: the same
    -- artifact is also recorded on the brief's campaign row (see internal/service's clone
    -- path) so the ordinary campaign surfaces show it without reading this table.
    email_id        TEXT,
    draft_url       TEXT,
    -- chat_history is persisted, which the originating service did NOT do. A chat turn
    -- sends the conversation so far back to the model as context; with the history in
    -- process memory, a pod change or restart between two turns silently drops that context
    -- and the model answers the second turn as if the first never happened. That failure is
    -- invisible -- the request succeeds, the answer is just wrong -- which is exactly the
    -- kind that must not be left to luck. DEFAULT '[]' (not NULL) so a reader never has to
    -- distinguish "no turns yet" from "column unset".
    chat_history    JSONB       NOT NULL DEFAULT '[]',
    -- version is the optimistic-concurrency guard, matching campaign_briefs/campaigns/
    -- campaign_audiences. Two browser tabs on the same session are an ordinary occurrence
    -- here, and a last-write-wins update would let one tab's content generation silently
    -- overwrite the other's; UpdateSession gates on this column and the service maps the
    -- mismatch to 409.
    version         BIGINT      NOT NULL DEFAULT 1,
    -- created_by / updated_by are JSONB actor blobs, matching campaign_briefs and
    -- campaign_audiences, and NULLABLE for the same reason they are there: nil means "no
    -- authenticated principal was recorded", which is a real state the audit trail must be
    -- able to express rather than one an INSERT refuses.
    created_by      JSONB,
    updated_by      JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- COMPOSITE parent FK, not a brief_id-only one, for the reason 000007 gives and 000028
    -- repeats: this row COPIES project_id and every read trusts that copy for tenant
    -- scoping. A brief_id-only FK would prove the brief exists while leaving the copied
    -- project_id unchecked, so a direct writer could persist a session whose project_id
    -- names a different project than its brief -- and it would then read out under the
    -- wrong tenant. The referenced UNIQUE (id, project_id) on campaign_briefs was added by
    -- 000007; depending on it means 000007's DOWN migration cannot run while this table
    -- exists.
    FOREIGN KEY (brief_id, project_id) REFERENCES campaign_briefs (id, project_id)
);

-- Sessions are listed per brief (the UI offers "resume a run"), so brief_id is indexed on
-- its own. Unlike creative_assets there is no composite unique key whose leftmost column
-- would already cover it.
CREATE INDEX IF NOT EXISTS idx_wizard_sessions_brief ON wizard_sessions (brief_id);

-- PARTIAL index: the SSE handler's cross-pod fallback looks a session up by its progress
-- token on every heartbeat tick, and most rows have no token at all. Excluding the NULLs
-- keeps the index proportional to the sessions that are actually being streamed rather
-- than to the table.
CREATE INDEX IF NOT EXISTS idx_wizard_sessions_progress_token
    ON wizard_sessions (progress_token) WHERE progress_token IS NOT NULL;

-- GROWTH -- recorded here for the same reason 000028 records it, because nothing bounds
-- this table either. A wizard session is never deleted: briefs are soft-archived rather
-- than removed, so there is no orphan path and no ON DELETE clause to choose, and an
-- archived brief's sessions are retained forever. The rows are small (text plus a few
-- JSONB blobs, no blobs of bytes) and are created only by a human driving the wizard, so
-- the growth rate is bounded by human activity rather than by traffic -- which is why a
-- prune is not shipped here. If that changes, the prune belongs next to
-- PruneTerminalJobs, keyed on terminal phase plus age, with the same allow-list
-- discipline: 'complete' is the only phase that is safely disposable, and an OLD
-- 'planning' row is a session someone abandoned mid-run, which is the record worth
-- keeping.

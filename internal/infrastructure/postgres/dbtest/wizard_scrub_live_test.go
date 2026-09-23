// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// sameJSON compares two JSON documents by VALUE. JSONB stores a parsed representation and
// re-serializes on read, so the bytes that come back differ from the bytes that went in even
// when nothing changed.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Errorf("value read back is not valid JSON: %v", err)
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("test expectation is not valid JSON: %v", err)
	}
	return reflect.DeepEqual(g, w)
}

func newWizardSessionRepo(pool *pgxpool.Pool) *postgres.WizardSessionRepo {
	return postgres.NewWizardSessionRepo(&postgres.Pool{Pool: pool})
}

// TestLiveScrubSessionsForBriefClearsOnlyThatBriefsPersonalData exercises the retention scrub
// against the real migrated schema. A service-level test with an in-memory fake cannot reach
// any of what decides whether this statement is safe:
//
//   - `chat_history` is JSONB NOT NULL DEFAULT '[]', so the scrubbed value has to be '[]' and
//     not NULL; a fake accepts whatever Go zero value the author picked.
//   - the `chat_history <> '[]'::jsonb` guard is a JSONB comparison, which is a real operator
//     with real semantics, not a Go `!=`.
//   - the WHERE is the tenancy boundary. This is a DESTRUCTIVE UPDATE with no undo: if the two
//     bind arguments landed on the wrong columns, the first production run would clear another
//     project's sessions and nothing would report it.
//
// The seed covers both survivor shapes: a same-project/different-brief row and a
// different-project row must both come through untouched.
//
// Only the brief_id predicate is load-bearing against them, and that is a property of the
// SCHEMA rather than of this statement: campaign_briefs.id is the primary key, so a brief id
// is globally unique, and wizard_sessions carries a COMPOSITE FK (brief_id, project_id) that
// forces a session's project to match its brief's. A cross-tenant row sharing the target's
// brief id therefore cannot be inserted -- verified against the live schema, which refuses
// the duplicate id outright.
//
// Stated plainly because the opposite reading is the dangerous one: removing `project_id = $1`
// does NOT fail this test, and that is correct, not a gap. The predicate is defence in depth
// against a future schema where brief ids are only unique per project -- the shape 000007's
// UNIQUE (id, project_id) already anticipates -- and against a caller that passes a brief id
// from one tenant with another tenant's project id. Keep it: this is a destructive UPDATE with
// no undo, and the cost of the extra predicate is nothing.
func TestLiveScrubSessionsForBriefClearsOnlyThatBriefsPersonalData(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	projectID := dbtest.UniqueID(t, "proj-scrub")
	otherProject := dbtest.UniqueID(t, "proj-other")

	target, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "target")), nil)
	if err != nil {
		t.Fatalf("CreateBrief (target): %v", err)
	}
	sibling, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "sibling")), nil)
	if err != nil {
		t.Fatalf("CreateBrief (sibling): %v", err)
	}
	// Same project id is not available across tenants, so the cross-project row gets its own
	// brief -- which is exactly the real shape: brief ids are unique per project.
	foreign, err := briefs.CreateBrief(ctx, draftBrief(otherProject, dbtest.UniqueID(t, "foreign")), nil)
	if err != nil {
		t.Fatalf("CreateBrief (foreign): %v", err)
	}

	actor := &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"}
	seed := func(projectID, briefID string) *model.WizardSession {
		t.Helper()
		// EVERY content column is populated. Seeding only chat_history would let a scrub that
		// clears one column and misses four pass -- which is the exact defect review found.
		s, serr := sessions.CreateSession(ctx, &model.WizardSession{
			ProjectID:        projectID,
			BriefID:          briefID,
			ChatHistory:      json.RawMessage(`[{"role":"user","content":"my unpublished keynote"}]`),
			PlanResult:       json.RawMessage(`{"extra_context":"write it for sponsors"}`),
			ReferenceVariant: json.RawMessage(`{"body":"generated reference copy"}`),
			StageVariant:     json.RawMessage(`{"body":"generated stage copy"}`),
			Sections:         json.RawMessage(`[{"type":"rich_text","html":"<p>edited</p>"}]`),
			CreatedBy:        actor,
			UpdatedBy:        actor,
		})
		if serr != nil {
			t.Fatalf("CreateSession(%s/%s): %v", projectID, briefID, serr)
		}
		return s
	}

	victim := seed(projectID, target.ID)
	siblingSession := seed(projectID, sibling.ID)
	foreignSession := seed(otherProject, foreign.ID)

	n, err := sessions.ScrubSessionsForBrief(ctx, projectID, target.ID)
	if err != nil {
		t.Fatalf("ScrubSessionsForBrief: %v", err)
	}
	if n != 1 {
		t.Errorf("scrubbed %d rows, want exactly 1 -- a different count means the WHERE reached rows it should not, or missed the one it should", n)
	}

	got, err := sessions.GetSession(ctx, projectID, target.ID, victim.ID)
	if err != nil {
		t.Fatalf("GetSession (victim): %v", err)
	}
	// '[]' rather than NULL: the column is NOT NULL, so a NULL scrub would have errored --
	// but an empty Go slice would ALSO read as "cleared" here, which is why the raw JSON is
	// compared rather than just its length.
	if string(got.ChatHistory) != "[]" {
		t.Errorf("chat_history = %q, want %q -- the user's typed content is retained", got.ChatHistory, "[]")
	}
	// plan_result carries the operator's own free-text guidance; the other three hold the
	// generated and hand-edited email bodies. A scrub that cleared only the transcript would
	// leave the same words in four other columns and still report success.
	for _, c := range []struct {
		name string
		val  json.RawMessage
	}{
		{"plan_result", got.PlanResult},
		{"reference_variant", got.ReferenceVariant},
		{"stage_variant", got.StageVariant},
		{"sections", got.Sections},
	} {
		if len(c.val) != 0 {
			t.Errorf("%s = %s, want empty -- content survived the delete", c.name, c.val)
		}
	}
	if got.CreatedBy != nil || got.UpdatedBy != nil {
		t.Errorf("actor blobs survived (created_by=%v updated_by=%v)", got.CreatedBy, got.UpdatedBy)
	}

	for _, keep := range []struct {
		label     string
		projectID string
		briefID   string
		id        string
	}{
		{"same project, different brief", projectID, sibling.ID, siblingSession.ID},
		{"different project", otherProject, foreign.ID, foreignSession.ID},
	} {
		survivor, gerr := sessions.GetSession(ctx, keep.projectID, keep.briefID, keep.id)
		if gerr != nil {
			t.Fatalf("GetSession (%s): %v", keep.label, gerr)
		}
		if string(survivor.ChatHistory) == "[]" || survivor.CreatedBy == nil || len(survivor.PlanResult) == 0 {
			t.Errorf("the scrub reached a session it must not touch (%s): chat_history=%q plan_result=%s created_by=%v",
				keep.label, survivor.ChatHistory, survivor.PlanResult, survivor.CreatedBy)
		}
	}

	// Idempotent: a second run matches nothing, because the guard excludes already-scrubbed
	// rows. Without it every repeat delete would rewrite rows and bump updated_at forever.
	again, err := sessions.ScrubSessionsForBrief(ctx, projectID, target.ID)
	if err != nil {
		t.Fatalf("ScrubSessionsForBrief (second run): %v", err)
	}
	if again != 0 {
		t.Errorf("second scrub touched %d rows, want 0 -- the already-scrubbed guard does not bind", again)
	}
}

// TestLiveScrubSessionsForBriefRejectsEmptyIdentifiers pins the guard against the shape that
// makes a destructive UPDATE dangerous: an empty bind argument. Postgres would happily run
// `WHERE project_id = ” AND brief_id = ”` and report zero rows, so the failure is silent --
// the caller believes it scrubbed.
func TestLiveScrubSessionsForBriefRejectsEmptyIdentifiers(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	sessions := newWizardSessionRepo(pool)

	for _, tc := range []struct{ name, projectID, briefID string }{
		{"empty project", "", "b1"},
		{"empty brief", "p1", ""},
		{"both empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sessions.ScrubSessionsForBrief(ctx, tc.projectID, tc.briefID); err == nil {
				t.Error("an empty identifier must be refused, not run as a zero-row UPDATE")
			} else if err != domain.ErrNotFound {
				t.Errorf("err = %v, want domain.ErrNotFound", err)
			}
		})
	}
}

// TestLiveWizardSessionCRUDRoundTrip exercises the repository methods against a real database.
//
// wizard_session_repo_test.go asserts over SQL SOURCE TEXT -- column order, actor stamping,
// tenant scoping in the query string. Those tests are worth having and they cannot fail for
// any of the reasons this one can: whether Postgres accepts the statement, whether the bind
// arguments land on the columns they name, whether the RETURNING list round-trips through
// seven JSONB columns, and whether the version gate actually gates.
func TestLiveWizardSessionCRUDRoundTrip(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	projectID := dbtest.UniqueID(t, "proj-crud")
	brief, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "crud")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	created, err := sessions.CreateSession(ctx, &model.WizardSession{
		ProjectID:        projectID,
		BriefID:          brief.ID,
		ProgressToken:    dbtest.UniqueID(t, "tok"),
		PlanResult:       json.RawMessage(`{"plan":"a"}`),
		ReferenceVariant: json.RawMessage(`{"variant":"reference"}`),
		StageVariant:     json.RawMessage(`{"variant":"stage"}`),
		Sections:         json.RawMessage(`[{"type":"rich_text"}]`),
		ChatHistory:      json.RawMessage(`[{"role":"user"}]`),
		EmailID:          "email-1",
		DraftURL:         "https://hubspot.example.test/draft/1",
		CreatedBy:        &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateSession returned no id")
	}
	if created.Version != 1 {
		t.Errorf("new session version = %d, want 1 (a database default, so this is the only place it is checked)", created.Version)
	}

	// Each JSONB column read back under its OWN name. A positional shift inside
	// scanWizardSession cannot fail at the type level -- five of these are JSONB and would
	// happily swap -- so the values are made distinguishable on purpose.
	got, err := sessions.GetSession(ctx, projectID, brief.ID, created.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// Compared as JSON values, not as bytes: JSONB is a parsed representation, so Postgres
	// returns `{"plan": "a"}` for the `{"plan":"a"}` that went in. A byte comparison here
	// fails on the whitespace and says nothing about the column mapping, which is what this
	// is actually checking.
	for _, f := range []struct{ name, got, want string }{
		{"plan_result", string(got.PlanResult), `{"plan":"a"}`},
		{"reference_variant", string(got.ReferenceVariant), `{"variant":"reference"}`},
		{"stage_variant", string(got.StageVariant), `{"variant":"stage"}`},
		{"sections", string(got.Sections), `[{"type":"rich_text"}]`},
		{"chat_history", string(got.ChatHistory), `[{"role":"user"}]`},
	} {
		if !sameJSON(t, f.got, f.want) {
			t.Errorf("%s = %s, want %s -- a swapped scan destination reads as valid JSON under the wrong name", f.name, f.got, f.want)
		}
	}
	for _, f := range []struct{ name, got, want string }{
		{"email_id", got.EmailID, "email-1"},
		{"draft_url", got.DraftURL, "https://hubspot.example.test/draft/1"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
	if got.CreatedBy == nil || got.CreatedBy.Email != "ada@example.test" {
		t.Errorf("created_by = %v, want the stamped actor", got.CreatedBy)
	}

	// Tenancy: the same id under another project must NOT resolve.
	if _, gerr := sessions.GetSession(ctx, dbtest.UniqueID(t, "proj-wrong"), brief.ID, created.ID); gerr != domain.ErrNotFound {
		t.Errorf("cross-project GetSession err = %v, want domain.ErrNotFound", gerr)
	}

	got.Phase = model.WizardPhaseContent
	got.ChatHistory = json.RawMessage(`[{"role":"user"},{"role":"assistant"}]`)
	got.UpdatedBy = &model.Actor{Name: "Grace Hopper", Email: "grace@example.test"}
	updated, err := sessions.UpdateSession(ctx, got, created.Version)
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	if updated.Version != created.Version+1 {
		t.Errorf("version = %d, want %d -- the optimistic-concurrency counter must advance", updated.Version, created.Version+1)
	}

	// The version gate. A second update at the ORIGINAL version is the two-tabs case: it must
	// be refused as STALE, not reported as missing. The distinction decides whether the caller
	// re-reads and retries or starts a second wizard session -- and a second session that
	// reaches the clone phase produces a duplicate HubSpot draft.
	if _, uerr := sessions.UpdateSession(ctx, got, created.Version); uerr != domain.ErrStaleWizardSession {
		t.Errorf("stale update err = %v, want domain.ErrStaleWizardSession", uerr)
	}

	// A row that does not exist must classify as NotFound, not Stale -- the other arm of the
	// same branch, which a test of only the stale case would leave unproven.
	missing := *got
	missing.ID = "00000000-0000-0000-0000-000000000000"
	if _, uerr := sessions.UpdateSession(ctx, &missing, 1); uerr != domain.ErrNotFound {
		t.Errorf("update of a missing session err = %v, want domain.ErrNotFound", uerr)
	}
}

// TestLiveGetSessionByTokenIsDeterministicOnATie pins the tiebreaker in
// `ORDER BY created_at DESC, id DESC`.
//
// created_at defaults to now(), which is the TRANSACTION timestamp -- so two sessions created
// inside one transaction do not merely land close together, they tie EXACTLY. With only
// created_at in the ORDER BY, the winner is whatever the plan returns first: stable-looking,
// and free to change under a different plan or after a vacuum. The SSE handler resolves a
// token through this lookup, so a non-deterministic pick routes one caller's progress frames
// from the wrong session's run.
//
// The tie is forced rather than hoped for: both rows are inserted in a single transaction and
// their created_at is then asserted EQUAL, so the test cannot quietly degrade into two
// distinct timestamps that the tiebreaker never has to break.
func TestLiveGetSessionByTokenIsDeterministicOnATie(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	projectID := dbtest.UniqueID(t, "proj-tie")
	brief, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "tie")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	token := dbtest.UniqueID(t, "shared-token")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids := make([]string, 0, 2)
	for range 2 {
		var id string
		if qerr := tx.QueryRow(ctx,
			`INSERT INTO wizard_sessions (project_id, brief_id, phase, progress_token)
			 VALUES ($1, $2, 'planning', $3) RETURNING id`,
			projectID, brief.ID, token).Scan(&id); qerr != nil {
			t.Fatalf("seed insert: %v", qerr)
		}
		ids = append(ids, id)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		t.Fatalf("Commit: %v", cerr)
	}

	// Prove the tie is real. Without this the test could pass for the wrong reason.
	var tied bool
	if qerr := pool.QueryRow(ctx,
		`SELECT count(DISTINCT created_at) = 1 FROM wizard_sessions WHERE progress_token = $1`,
		token).Scan(&tied); qerr != nil {
		t.Fatalf("tie check: %v", qerr)
	}
	if !tied {
		t.Fatal("the two seeded sessions do not share created_at -- the tiebreaker is never exercised, so this test proves nothing")
	}

	want := max(ids[0], ids[1])
	for i := range 5 {
		got, gerr := sessions.GetSessionByToken(ctx, token)
		if gerr != nil {
			t.Fatalf("GetSessionByToken (run %d): %v", i, gerr)
		}
		if got.ID != want {
			t.Fatalf("run %d resolved the token to %s, want %s -- the pick is not deterministic on a created_at tie", i, got.ID, want)
		}
	}
}

// TestLiveScrubSurvivesAnInFlightTurn pins the scrub against being silently UNDONE.
//
// Clearing the columns is not enough on its own. UpdateSession gates on `version = $14`, so a
// scrub that left the counter alone would leave any in-flight turn's pre-scrub snapshot still
// matching -- and generate-content holds exactly such a snapshot across a long model call. The
// turn would then write the transcript and actor blobs back over the cleared columns, with the
// brief still archived and nothing scheduled to purge them again. The delete would report
// success, the data would be gone for a second, and it would come back.
//
// The fix is a `version + 1` in the scrub, which lets the session's existing
// optimistic-concurrency protocol refuse the stale write -- the correct answer, because the
// caller's snapshot genuinely IS stale.
func TestLiveScrubSurvivesAnInFlightTurn(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	projectID := dbtest.UniqueID(t, "proj-inflight")
	brief, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "inflight")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	created, err := sessions.CreateSession(ctx, &model.WizardSession{
		ProjectID:   projectID,
		BriefID:     brief.ID,
		ChatHistory: json.RawMessage(`[{"role":"user","content":"my unpublished keynote"}]`),
		CreatedBy:   &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The snapshot a turn already holds when the delete arrives.
	inFlight, err := sessions.GetSession(ctx, projectID, brief.ID, created.ID)
	if err != nil {
		t.Fatalf("GetSession (in-flight snapshot): %v", err)
	}

	if _, serr := sessions.ScrubSessionsForBrief(ctx, projectID, brief.ID); serr != nil {
		t.Fatalf("ScrubSessionsForBrief: %v", serr)
	}

	// The turn completes and tries to persist what it loaded.
	if _, uerr := sessions.UpdateSession(ctx, inFlight, inFlight.Version); uerr != domain.ErrStaleWizardSession {
		t.Errorf("late write err = %v, want domain.ErrStaleWizardSession -- an accepted write restores the scrubbed transcript", uerr)
	}

	after, err := sessions.GetSession(ctx, projectID, brief.ID, created.ID)
	if err != nil {
		t.Fatalf("GetSession (after): %v", err)
	}
	if !sameJSON(t, string(after.ChatHistory), "[]") {
		t.Errorf("chat_history = %s, want [] -- the in-flight turn restored the personal data the delete removed", after.ChatHistory)
	}
	if after.CreatedBy != nil || after.UpdatedBy != nil {
		t.Errorf("actor blobs came back (created_by=%v updated_by=%v)", after.CreatedBy, after.UpdatedBy)
	}
}

// TestLiveCreateSessionRefusesAnArchivedBrief pins the gate that closes the OTHER way a
// deleted brief can keep personal data.
//
// StartEmailWizardPlan reads the brief before inserting, but that read cannot carry the
// guarantee: the composite FK only requires the brief ROW to exist, and ArchiveBrief is a SOFT
// delete that leaves it. An archive landing between the check and the insert therefore still
// satisfies the FK, and the new session -- with its chat_history and actor blobs -- is created
// against a brief the operator has deleted, AFTER the scrub has already run past it. Nothing
// revisits it.
//
// Closing that window needs the parent check inside the INSERT, which is what
// createWizardSessionQuery's WHERE EXISTS does. The refusal must be ErrNotFound rather than a
// raw error, because the brief genuinely is gone -- a 500 would tell the caller to retry
// something that can never succeed.
func TestLiveCreateSessionRefusesAnArchivedBrief(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	projectID := dbtest.UniqueID(t, "proj-archived")
	brief, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "archived")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// An ACTIVE parent still accepts sessions -- without this the test would pass against a
	// gate that refuses everything.
	if _, cerr := sessions.CreateSession(ctx, &model.WizardSession{
		ProjectID: projectID, BriefID: brief.ID,
	}); cerr != nil {
		t.Fatalf("a session on an ACTIVE brief must be accepted, got %v", cerr)
	}

	if _, aerr := briefs.ArchiveBrief(ctx, projectID, brief.ID, &model.Actor{Name: "Ada Lovelace"}, nil); aerr != nil {
		t.Fatalf("ArchiveBrief: %v", aerr)
	}
	if _, serr := sessions.ScrubSessionsForBrief(ctx, projectID, brief.ID); serr != nil {
		t.Fatalf("ScrubSessionsForBrief: %v", serr)
	}

	// The racing plan-start, whose own GetBrief succeeded just before the archive.
	_, cerr := sessions.CreateSession(ctx, &model.WizardSession{
		ProjectID:   projectID,
		BriefID:     brief.ID,
		ChatHistory: json.RawMessage(`[{"role":"user","content":"late secret"}]`),
		CreatedBy:   &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"},
	})
	if cerr != domain.ErrNotFound {
		t.Fatalf("CreateSession against an archived brief err = %v, want domain.ErrNotFound -- a retained session survives the delete", cerr)
	}

	// And nothing was written: a gate that inserted and then errored would be worse than none.
	var n int
	if qerr := pool.QueryRow(ctx,
		`SELECT count(*) FROM wizard_sessions WHERE brief_id = $1 AND chat_history <> '[]'::jsonb`,
		brief.ID).Scan(&n); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if n != 0 {
		t.Errorf("%d session(s) still hold chat_history for a deleted brief", n)
	}
}

// TestLiveCreateSessionSerializesAgainstArchive pins the FOR UPDATE lock in CreateSession.
//
// The insert's own WHERE EXISTS closes the check-then-insert gap WITHIN one statement, but two
// statements in different transactions still interleave: under READ COMMITTED an ArchiveBrief
// can commit after the subquery observed `status <> 'archived'` and run its scrub before the
// insert commits. The foreign key is still satisfied -- the soft delete keeps the parent row --
// so a brand-new unscrubbed session is left behind a completed deletion.
//
// This drives the interleaving rather than hoping for it: a concurrent goroutine archives while
// the insert is in flight, and the test asserts the two outcomes are CONSISTENT. Either the
// insert won (and the session exists, having been created before the delete) or the archive won
// (and the insert was refused). What must never happen is both: an archived brief with a
// populated session created after it.
func TestLiveCreateSessionSerializesAgainstArchive(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	// Repeated, because a race that is merely UNLIKELY passes a single run. With the lock
	// removed this fails within the first few iterations.
	for i := range 12 {
		projectID := dbtest.UniqueID(t, "proj-race")
		brief, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "race")), nil)
		if err != nil {
			t.Fatalf("CreateBrief: %v", err)
		}

		var wg sync.WaitGroup
		var createErr, archiveErr error
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, createErr = sessions.CreateSession(ctx, &model.WizardSession{
				ProjectID:   projectID,
				BriefID:     brief.ID,
				ChatHistory: json.RawMessage(`[{"role":"user","content":"racing secret"}]`),
				CreatedBy:   &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, archiveErr = briefs.ArchiveBrief(ctx, projectID, brief.ID,
				&model.Actor{Name: "Ada Lovelace"}, nil); archiveErr != nil {
				return
			}
			_, archiveErr = sessions.ScrubSessionsForBrief(ctx, projectID, brief.ID)
		}()
		close(start)
		wg.Wait()

		if archiveErr != nil {
			t.Fatalf("iteration %d: archive/scrub: %v", i, archiveErr)
		}

		// Whatever the interleaving, no session may hold content for this archived brief.
		var leaked int
		if qerr := pool.QueryRow(ctx,
			`SELECT count(*) FROM wizard_sessions WHERE brief_id = $1 AND chat_history <> '[]'::jsonb`,
			brief.ID).Scan(&leaked); qerr != nil {
			t.Fatalf("iteration %d: count: %v", i, qerr)
		}
		if leaked != 0 {
			t.Fatalf("iteration %d: %d session(s) hold chat_history for a brief that was archived and scrubbed "+
				"(CreateSession err=%v) -- the insert straddled the archive", i, leaked, createErr)
		}
	}
}

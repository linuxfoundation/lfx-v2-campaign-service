// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// WizardSessionRepo is a pgx-backed implementation of domain.WizardSessionRepository.
type WizardSessionRepo struct {
	db *Pool
}

// NewWizardSessionRepo returns a WizardSessionRepo backed by pool.
func NewWizardSessionRepo(pool *Pool) *WizardSessionRepo { return &WizardSessionRepo{db: pool} }

var _ domain.WizardSessionRepository = (*WizardSessionRepo)(nil)

const wizardSessionCols = `id::text, project_id::text, brief_id::text, phase, progress_token,
	plan_result, reference_variant, stage_variant, sections, email_id, draft_url, chat_history,
	version, created_by, updated_by, created_at, updated_at`

// Both write statements are package constants for the reason brief_repo.go's are: each
// one stamps an actor column in the SAME statement as the write it performs, and a
// follow-up UPDATE to do the attribution separately would compile, pass, and leave a
// committed window where the row had changed and the audit trail had not.
const (
	// INSERT ... SELECT ... WHERE EXISTS, matching createCreativeAssetQuery: the parent-brief
	// check is part of THIS statement rather than a separate read before it.
	//
	// The caller does read the brief first, but that read cannot carry the guarantee. The
	// composite FK only requires the brief ROW to exist, and ArchiveBrief is a SOFT delete --
	// it sets status='archived' and leaves the row -- so an archive landing between the check
	// and this insert still satisfies the FK. The session is then created against a brief the
	// operator has deleted, carrying chat_history and actor blobs that the deletion scrub has
	// already run past and will never revisit. Verified against a live database: without this
	// gate, the insert succeeds.
	//
	// Zero rows means the brief is absent, archived, or another project's; CreateSession maps
	// that to ErrNotFound, which is also the right answer for the race -- the brief is gone.
	createWizardSessionQuery = `INSERT INTO wizard_sessions
		(project_id, brief_id, phase, progress_token, plan_result, reference_variant, stage_variant,
		 sections, email_id, draft_url, chat_history, created_by, updated_by)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,COALESCE($11,'[]'::jsonb),$12,$12
		WHERE EXISTS (
			SELECT 1 FROM campaign_briefs
			WHERE id = $2 AND project_id = $1 AND status <> 'archived'
		)
		RETURNING ` + wizardSessionCols

	// The SET list is exactly the mutable state: identity (id/project_id/brief_id) and
	// provenance (created_by/created_at) are fixed at creation, so they are absent here
	// rather than set to themselves — a column that cannot change must not appear in an
	// UPDATE, or a future caller will assume it can.
	//
	// progress_token IS updatable: a caller may re-plan a session under a fresh token,
	// and leaving the old one would point the SSE fallback at a stream nobody reads.
	updateWizardSessionQuery = `UPDATE wizard_sessions SET
		phase=$1, progress_token=$2, plan_result=$3, reference_variant=$4, stage_variant=$5,
		sections=$6, email_id=$7, draft_url=$8, chat_history=COALESCE($9,'[]'::jsonb),
		updated_by=$10, version=version+1, updated_at=now()
		WHERE id=$11 AND project_id=$12 AND brief_id=$13 AND version=$14
		RETURNING ` + wizardSessionCols
)

// CreateSession inserts a wizard session and returns the stored row.
//
// Returns domain.ErrNotFound when the parent brief is missing OR archived. The composite FK only
// requires the row to EXIST, and ArchiveBrief is a SOFT delete that leaves it there, so the FK
// alone would admit a session against a brief the operator had just deleted.
//
// Runs in a transaction that takes SELECT ... FOR UPDATE on the brief row, which serialises this
// against a concurrent ArchiveBrief: under READ COMMITTED a WHERE EXISTS check could pass while
// the archive committed between the check and the insert.
func (r *WizardSessionRepo) CreateSession(ctx context.Context, s *model.WizardSession) (*model.WizardSession, error) {
	createdBy, err := marshalActor(s.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("create wizard session: %w", err)
	}
	// Lock the parent brief, then insert on the SAME transaction -- the shape
	// CreateAsset uses, and for the same reason. The insert's own WHERE EXISTS closes the
	// check-then-insert gap WITHIN one statement, but two statements in different
	// transactions still interleave: under READ COMMITTED an ArchiveBrief can commit after
	// this subquery observed `status <> 'archived'` and run its scrub before this insert
	// commits. The FK is still satisfied, because the soft delete keeps the parent row -- so
	// a brand-new unscrubbed session is left behind a completed deletion.
	//
	// FOR UPDATE is what orders the two: whichever transaction takes the lock first runs to
	// completion before the other observes the row, so the archive either waits and then
	// scrubs this session, or wins and makes this insert return no rows.
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create wizard session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	lockQ := `SELECT status FROM campaign_briefs WHERE id = $1 AND project_id = $2 FOR UPDATE`
	if lerr := tx.QueryRow(ctx, lockQ, s.BriefID, s.ProjectID).Scan(&status); lerr != nil {
		if errors.Is(lerr, pgx.ErrNoRows) {
			// Absent, or another project's. Both are ErrNotFound for the reason the insert's
			// gate collapses them too: telling them apart leaks whether a brief the caller
			// cannot see exists.
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("create wizard session: lock brief: %w", lerr)
	}
	if status == "archived" {
		return nil, domain.ErrNotFound
	}

	row := tx.QueryRow(ctx, createWizardSessionQuery,
		s.ProjectID, s.BriefID, string(s.PhaseOrDefault()), nullStr(s.ProgressToken),
		nullJSON(s.PlanResult), nullJSON(s.ReferenceVariant), nullJSON(s.StageVariant),
		nullJSON(s.Sections), nullStr(s.EmailID), nullStr(s.DraftURL), nullJSON(s.ChatHistory),
		createdBy,
	)
	out, err := scanWizardSession(row)
	if err != nil {
		// No row means the WHERE EXISTS gate found no ACTIVE parent brief: absent, archived,
		// or another project's. That is a 404, not a 500 -- the same answer GetBrief would
		// have given, and the correct one for the archive race the gate closes.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("create wizard session: %w", err)
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("create wizard session: commit: %w", cerr)
	}
	return out, nil
}

// GetSession returns one session scoped to its project and brief.
func (r *WizardSessionRepo) GetSession(ctx context.Context, projectID, briefID, id string) (*model.WizardSession, error) {
	q := `SELECT ` + wizardSessionCols + ` FROM wizard_sessions
		WHERE id = $1 AND project_id = $2 AND brief_id = $3`
	s, err := scanWizardSession(r.db.QueryRow(ctx, q, id, projectID, briefID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get wizard session: %w", err)
	}
	return s, nil
}

// GetSessionByToken returns the session holding progressToken.
//
// The empty token is refused BEFORE the query rather than passed through. Matching
// progress_token against the empty string would find nothing today, but the check is not
// about what the column happens to contain: this is the one lookup with no tenant scope,
// so an empty-token caller must be stopped by a rule rather than by the current data.
func (r *WizardSessionRepo) GetSessionByToken(ctx context.Context, progressToken string) (*model.WizardSession, error) {
	if progressToken == "" {
		return nil, domain.ErrNotFound
	}
	// ORDER BY created_at DESC, id DESC LIMIT 1: the token is not UNIQUE (see 000033 — a
	// caller may legitimately reuse its own token across a re-plan), so the newest holder is
	// the one whose run is actually streaming. Without the ordering the result would be
	// whatever the planner happened to return first, which is a stable-looking answer that
	// changes under vacuum.
	//
	// `id` is the TIEBREAKER, and it is not decoration: created_at defaults to now(), which
	// is the TRANSACTION timestamp, so two sessions created in one transaction tie EXACTLY
	// rather than merely close together. On a tie the SSE handler would resolve a token to a
	// non-deterministic session, and the pick could change between executions or plans. The
	// column is a primary key, so it totally orders whatever created_at leaves tied.
	//
	// The token is server-minted (email_wizard.go), so a collision across projects is not
	// reachable; the handler's project/brief check remains the authorization boundary either
	// way. This ordering is about determinism, not authorization.
	q := `SELECT ` + wizardSessionCols + ` FROM wizard_sessions
		WHERE progress_token = $1 ORDER BY created_at DESC, id DESC LIMIT 1`
	s, err := scanWizardSession(r.db.QueryRow(ctx, q, progressToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get wizard session by token: %w", err)
	}
	return s, nil
}

// UpdateSession replaces a session's mutable fields, gating on expectedVersion.
//
// The update runs inside a transaction so a zero-row result can be CLASSIFIED before the
// snapshot moves: outside one, the follow-up "does this row exist?" read could observe a
// concurrent delete or a second update and report the wrong reason for the same failure.
func (r *WizardSessionRepo) UpdateSession(ctx context.Context, s *model.WizardSession, expectedVersion int64) (*model.WizardSession, error) {
	updatedBy, err := marshalActor(s.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("update wizard session: %w", err)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("update wizard session: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, updateWizardSessionQuery,
		string(s.PhaseOrDefault()), nullStr(s.ProgressToken),
		nullJSON(s.PlanResult), nullJSON(s.ReferenceVariant), nullJSON(s.StageVariant),
		nullJSON(s.Sections), nullStr(s.EmailID), nullStr(s.DraftURL), nullJSON(s.ChatHistory),
		updatedBy, s.ID, s.ProjectID, s.BriefID, expectedVersion,
	)
	out, err := scanWizardSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, classifyWizardSessionNoRowTx(ctx, tx, s.ProjectID, s.BriefID, s.ID)
		}
		return nil, fmt.Errorf("update wizard session: %w", err)
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("update wizard session: commit: %w", cerr)
	}
	return out, nil
}

// classifyWizardSessionNoRowTx decides WHY a version-gated update matched no row, using
// the same transaction (and therefore the same snapshot) as the update itself.
//
// This mirrors classifyNoRowTx for briefs. The distinction matters to the caller and is
// not cosmetic: a stale version means "another turn of this session landed first, re-read
// and retry", while a missing row means "this session is gone, start a new one". A caller
// given the second answer for the first situation starts a SECOND wizard session for the
// same run — and if that run had already reached the clone phase, the duplicate produces a
// second HubSpot draft.
func classifyWizardSessionNoRowTx(ctx context.Context, tx pgx.Tx, projectID, briefID, id string) error {
	var exists bool
	q := `SELECT EXISTS (SELECT 1 FROM wizard_sessions WHERE id = $1 AND project_id = $2 AND brief_id = $3)`
	if err := tx.QueryRow(ctx, q, id, projectID, briefID).Scan(&exists); err != nil {
		return fmt.Errorf("update wizard session: classify: %w", err)
	}
	if exists {
		return domain.ErrStaleWizardSession
	}
	return domain.ErrNotFound
}

func scanWizardSession(row pgx.Row) (*model.WizardSession, error) {
	var (
		s                                                    model.WizardSession
		phase                                                string
		progressToken, emailID, draftURL                     *string
		planResult, referenceVariant, stageVariant, sections []byte
		chatHistory                                          []byte
		createdBy, updatedBy                                 []byte
	)
	if err := row.Scan(
		&s.ID, &s.ProjectID, &s.BriefID, &phase, &progressToken,
		&planResult, &referenceVariant, &stageVariant, &sections,
		&emailID, &draftURL, &chatHistory,
		&s.Version, &createdBy, &updatedBy, &s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	s.Phase = model.WizardPhase(phase)
	s.ProgressToken = derefStr(progressToken)
	s.EmailID = derefStr(emailID)
	s.DraftURL = derefStr(draftURL)
	s.PlanResult = rawJSON(planResult)
	s.ReferenceVariant = rawJSON(referenceVariant)
	s.StageVariant = rawJSON(stageVariant)
	s.Sections = rawJSON(sections)
	s.ChatHistory = rawJSON(chatHistory)

	// Corrupt actor JSON is surfaced rather than silently yielding a nil audit trail —
	// same reasoning as scanBrief.
	var err error
	if s.CreatedBy, err = unmarshalActor(createdBy); err != nil {
		return nil, fmt.Errorf("scan wizard session: unmarshal created_by: %w", err)
	}
	if s.UpdatedBy, err = unmarshalActor(updatedBy); err != nil {
		return nil, fmt.Errorf("scan wizard session: unmarshal updated_by: %w", err)
	}
	return &s, nil
}

// derefStr renders a nullable text column as Go's zero value.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// rawJSON renders a nullable JSONB column as json.RawMessage, keeping SQL NULL and an
// empty blob indistinguishable in Go — nullJSON maps both back to NULL on write, so the
// round trip is stable.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// scrubWizardSessionsQuery clears personal data from a brief's sessions, keeping the rows.
//
// An explicit column list, not a DELETE: the row records that a wizard run happened, which is
// the audit trail worth keeping, while everything content-bearing goes. Scoped by BOTH project
// and brief, so a brief id alone cannot reach another project's rows.
//
// EVERY content column, not just chat_history. `plan_result` carries the operator's own
// free-text guidance (wizardPlan.ExtraContext, supplied on plan-start and plan), and
// `reference_variant`, `stage_variant` and `sections` hold the generated and hand-edited email
// bodies. Clearing only the transcript would have left the same words behind in four other
// columns and reported success -- a scrub that is partial is worse than one that is absent,
// because it looks done.
//
// The identity/provenance columns that REMAIN are deliberate: id, project_id, brief_id, phase,
// email_id, draft_url and the timestamps. They record that a run happened and which HubSpot
// draft it produced, which is the audit trail, and none of them is user-authored text.
//
// `version + 1` is what makes the scrub STICK, and it is not bookkeeping. UpdateSession gates
// on `version = $14`, so a scrub that left the counter alone would leave an in-flight turn's
// pre-scrub snapshot still matching -- and generate-content holds exactly such a snapshot
// across a long model call. That turn would then write the transcript and actor back over the
// cleared columns, restoring the personal data with the brief still archived and no later
// purge to catch it. Bumping the version makes the session's existing optimistic-concurrency
// protocol refuse the stale write as ErrStaleWizardSession, which is the correct answer: the
// caller's snapshot IS stale. Verified against a live database -- without the bump, the late
// write restores the transcript verbatim.
const scrubWizardSessionsQuery = `UPDATE wizard_sessions
	SET chat_history      = '[]'::jsonb,
	    plan_result       = NULL,
	    reference_variant = NULL,
	    stage_variant     = NULL,
	    sections          = NULL,
	    created_by        = NULL,
	    updated_by        = NULL,
	    version           = version + 1,
	    updated_at        = NOW()
	WHERE project_id = $1 AND brief_id = $2
	  AND (chat_history <> '[]'::jsonb
	       OR plan_result IS NOT NULL OR reference_variant IS NOT NULL
	       OR stage_variant IS NOT NULL OR sections IS NOT NULL
	       OR created_by IS NOT NULL OR updated_by IS NOT NULL)`

// ScrubSessionsForBrief implements domain.WizardSessionRepository.
func (r *WizardSessionRepo) ScrubSessionsForBrief(ctx context.Context, projectID, briefID string) (int64, error) {
	if projectID == "" || briefID == "" {
		return 0, domain.ErrNotFound
	}
	tag, err := r.db.Exec(ctx, scrubWizardSessionsQuery, projectID, briefID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

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

// BriefRepo is a pgx-backed implementation of domain.BriefRepository.
type BriefRepo struct {
	db *Pool
}

// NewBriefRepo returns a BriefRepo backed by pool.
func NewBriefRepo(pool *Pool) *BriefRepo { return &BriefRepo{db: pool} }

var _ domain.BriefRepository = (*BriefRepo)(nil)

const briefCols = `id::text, project_id::text, program_type, event_slug, url, platforms, event_details,
	copy, keywords, targeting, status, version, approved_by, approved_at, created_by, updated_by,
	created_at, updated_at`

// The four write statements are package constants rather than function locals so the
// invariant they share — every one of them stamps an actor column in the SAME statement
// as the write it performs — can be asserted by a test. Nothing else enforces it: a
// follow-up UPDATE would compile, pass every existing test, and leave a committed window
// in which the row changed and the attribution had not.
const (
	createBriefQuery = `INSERT INTO campaign_briefs
		(project_id, program_type, event_slug, url, platforms, event_details, copy, keywords, targeting,
		 approved_by, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11) RETURNING ` + briefCols

	// Replacing brief content invalidates any prior approval: reset the brief to
	// 'draft' and clear the approver so a modified brief cannot silently retain
	// status='approved' (which would let changed ad inputs be treated as approved
	// and dispatched without re-review). event_slug is included so a slug change
	// is actually persisted (it is subject to the partial-unique index, which
	// surfaces a conflict if the new slug collides with a live brief).
	replaceBriefQuery = `UPDATE campaign_briefs SET
		program_type=$1, event_slug=$2, url=$3, platforms=$4, event_details=$5, copy=$6, keywords=$7, targeting=$8,
		status='draft', approved_by=NULL, approved_at=NULL,
		updated_by=$9, version=version+1, updated_at=now()
		WHERE id=$10 AND project_id=$11 AND version=$12 AND status <> 'archived'
		RETURNING ` + briefCols

	approveBriefQuery = `UPDATE campaign_briefs SET status='approved', approved_by=$1, approved_at=now(),
		updated_by=$1, version=version+1, updated_at=now()
		WHERE id=$2 AND project_id=$3 AND version=$4 AND status <> 'archived'
		RETURNING ` + briefCols

	archiveBriefQuery = `UPDATE campaign_briefs SET status='archived', updated_by=$3, version=version+1, updated_at=now()
		WHERE id=$1 AND project_id=$2 AND status <> 'archived'
		RETURNING ` + briefCols
)

// GetBrief returns a non-archived brief by id scoped to the project.
func (r *BriefRepo) GetBrief(ctx context.Context, projectID, id string) (*model.CampaignBrief, error) {
	q := `SELECT ` + briefCols + ` FROM campaign_briefs WHERE id = $1 AND project_id = $2 AND status <> 'archived'`
	b, err := scanBrief(r.db.QueryRow(ctx, q, id, projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get brief: %w", err)
	}
	return b, nil
}

// ConfirmBriefApproved reports nil when the brief is still approved at expectedVersion,
// domain.ErrStaleApproval when it is not, and domain.ErrNotFound when it is missing or
// archived. See domain.BriefReader for why this is a repository operation rather than a
// GetBrief plus a comparison in the caller.
//
// The whole point is `FOR UPDATE`. A plain SELECT reads the last committed row, so a
// ReplaceBrief that has updated the row and not yet committed is invisible and the caller
// gets back the approval that is in the act of being withdrawn. The lock request queues
// behind that writer instead; when it commits, this read is re-evaluated against the
// writer's row and the version no longer matches. When the writer rolls back, the read
// proceeds against the unchanged row and the confirmation passes, which is also correct.
//
// The transaction WRITES NOTHING and is rolled back rather than committed — a rollback
// releases the lock exactly as a commit would. It is held for the length of one indexed
// lookup, so it never delays a brief mutation by anything measurable.
//
// "Writes nothing" and not "READ ONLY": the latter names a PostgreSQL transaction mode
// this transaction is NOT in (`Begin` uses the read-write default) and, more to the
// point, one it could not be — `SELECT ... FOR UPDATE` takes a row lock, which a
// genuinely READ ONLY transaction rejects. Do not "make the comment true" by adding
// pgx.TxOptions{AccessMode: pgx.ReadOnly}; it would break the lock this function exists
// for.
//
// The archived predicate matches GetBrief's: an archived brief is ErrNotFound, not a
// stale approval, because "refresh and rebuild" is the wrong instruction for a brief that
// is gone.
func (r *BriefRepo) ConfirmBriefApproved(ctx context.Context, projectID, id string, expectedVersion int64) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("confirm brief approved: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		status  model.BriefStatus
		version int64
	)
	q := `SELECT status, version FROM campaign_briefs
		WHERE id = $1 AND project_id = $2 AND status <> 'archived' FOR UPDATE`
	if serr := tx.QueryRow(ctx, q, id, projectID).Scan(&status, &version); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("confirm brief approved: %w", serr)
	}
	if status != model.BriefApproved || version != expectedVersion {
		return domain.ErrStaleApproval
	}
	return nil
}

// classifyNoRowTx decides whether a guarded UPDATE that matched no row was a MISSING brief or a
// STALE version, reading through the SAME transaction.
//
// It must not call GetBrief: that acquires a SECOND pool connection while this transaction still
// holds the first. With a saturated pool — pool_max_conns=1 makes it certain — an ordinary
// stale-version request would block until its context expired instead of returning 412. Reading
// inside the tx also sees the same snapshot the UPDATE did, so the two cannot disagree.
//
// A transient read error is surfaced as-is rather than masked as a precondition failure, which
// would make the caller retry with a fresh ETag instead of backing off on a server error.
func classifyNoRowTx(ctx context.Context, tx pgx.Tx, projectID, id string) error {
	var exists bool
	q := `SELECT EXISTS (SELECT 1 FROM campaign_briefs WHERE id = $1 AND project_id = $2 AND status <> 'archived')`
	if err := tx.QueryRow(ctx, q, id, projectID).Scan(&exists); err != nil {
		return fmt.Errorf("classify guarded update: %w", err)
	}
	if !exists {
		return domain.ErrNotFound
	}
	return domain.ErrPreconditionFailed
}

// FindBriefByEventSlug returns the non-archived brief for (projectID, eventSlug), or
// ErrNotFound when none exists. This is the "have I already generated a brief for this
// event?" lookup: the UI derives the slug from a pasted event URL and calls this before
// generating, so a previously generated brief (with its AI copy/keywords/targeting) is
// reused instead of silently regenerated.
//
// At most ONE row can match: uq_campaign_briefs_project_event is a UNIQUE index on
// (project_id, event_slug) WHERE status <> 'archived' — the same predicate used here, so
// this is an efficient unique-key lookup and archiving frees the slug for a fresh brief.
// (Not an index-ONLY scan: briefCols selects every column while the index carries just the
// two key columns, so the heap row is still fetched.)
func (r *BriefRepo) FindBriefByEventSlug(ctx context.Context, projectID, eventSlug string) (*model.CampaignBrief, error) {
	q := `SELECT ` + briefCols + ` FROM campaign_briefs WHERE project_id = $1 AND event_slug = $2 AND status <> 'archived'`
	b, err := scanBrief(r.db.QueryRow(ctx, q, projectID, eventSlug))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("find brief by event slug: %w", err)
	}
	return b, nil
}

// CreateBrief inserts a brief. Returns ErrConflict on UNIQUE(project_id, event_slug).
func (r *BriefRepo) CreateBrief(ctx context.Context, b *model.CampaignBrief, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	approvedBy, err := marshalActor(b.ApprovedBy)
	if err != nil {
		return nil, err
	}
	// created_by AND updated_by are both stamped on insert. Leaving updated_by NULL
	// until the first edit would make "who touched this last" unanswerable without
	// also consulting created_by, and every caller that asks does so to render one
	// name. The two diverge from the first update onwards, which is when it matters.
	createdBy, err := marshalActor(b.CreatedBy)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create brief: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, createBriefQuery,
		b.ProjectID, string(b.ProgramType), b.EventSlug, nullStr(b.URL),
		nullJSON(b.Platforms), nullJSON(b.EventDetails), nullJSON(b.Copy),
		nullJSON(b.Keywords), nullJSON(b.Targeting), approvedBy, createdBy,
	)
	created, err := scanBrief(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ErrConflict
		}
		return nil, fmt.Errorf("create brief: %w", err)
	}
	if eerr := enqueueBriefIndex(ctx, tx, created, indexPayload); eerr != nil {
		return nil, fmt.Errorf("create brief: %w", eerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("create brief: commit: %w", cerr)
	}
	return created, nil
}

// ReplaceBrief replaces mutable fields, gating on expectedVersion.
func (r *BriefRepo) ReplaceBrief(ctx context.Context, b *model.CampaignBrief, expectedVersion int64, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	updatedBy, err := marshalActor(b.UpdatedBy)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("replace brief: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// RETURNING the updated row keeps the write and the snapshot that gets indexed in ONE
	// statement, so the outbox payload is exactly what committed — the same reason ArchiveBrief
	// does it. The prior implementation re-read via GetBrief after the UPDATE, which could
	// observe a LATER concurrent write and index a snapshot this call never produced.
	updated, err := scanBrief(tx.QueryRow(ctx, replaceBriefQuery,
		string(b.ProgramType), b.EventSlug, nullStr(b.URL), nullJSON(b.Platforms), nullJSON(b.EventDetails),
		nullJSON(b.Copy), nullJSON(b.Keywords), nullJSON(b.Targeting),
		updatedBy, b.ID, b.ProjectID, expectedVersion,
	))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// A slug change that collides with another live brief trips the partial
		// unique index; surface it as a conflict (409) rather than a 500.
		if isUniqueViolation(err) {
			return nil, domain.ErrConflict
		}
		return nil, fmt.Errorf("replace brief: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish missing from stale version, THROUGH THIS TRANSACTION (see classifyNoRowTx).
		return nil, classifyNoRowTx(ctx, tx, b.ProjectID, b.ID)
	}
	if eerr := enqueueBriefIndex(ctx, tx, updated, indexPayload); eerr != nil {
		return nil, fmt.Errorf("replace brief: %w", eerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("replace brief: commit: %w", cerr)
	}
	return updated, nil
}

// Approve marks a brief approved, recording the actor.
func (r *BriefRepo) Approve(ctx context.Context, projectID, id string, by *model.Actor, expectedVersion int64, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	approvedBy, err := marshalActor(by)
	if err != nil {
		return nil, err
	}
	// Gate on version so a brief that was replaced (bumping its version) since the
	// approver fetched it cannot be approved on stale content.
	// Approving is a write, so it stamps updated_by as well. approved_by and updated_by
	// carry the same actor here and diverge afterwards: a later edit moves updated_by
	// while approved_by is cleared by ReplaceBrief, which is exactly the distinction
	// between "who signed off on this content" and "who touched the row last".
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("approve brief: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	approved, err := scanBrief(tx.QueryRow(ctx, approveBriefQuery, approvedBy, id, projectID, expectedVersion))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("approve brief: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish missing from stale version, mirroring ReplaceBrief.
		return nil, classifyNoRowTx(ctx, tx, projectID, id)
	}
	if eerr := enqueueBriefIndex(ctx, tx, approved, indexPayload); eerr != nil {
		return nil, fmt.Errorf("approve brief: %w", eerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("approve brief: commit: %w", cerr)
	}
	return approved, nil
}

// enqueueBriefIndex writes the brief's index message to the outbox inside tx.
//
// EVERY brief mutation goes through this — not just the terminal archive. A direct
// post-commit publish cannot be ordered against an outbox replay: a replace could commit,
// stall before publishing, and have its update land AFTER an archive was replayed and
// retired, resurrecting a deleted brief in the index. One ordered sequence per row removes
// that interleaving entirely.
//
// A nil builder means the caller does not want this resource indexed; the write still commits.
func enqueueBriefIndex(ctx context.Context, tx pgx.Tx, b *model.CampaignBrief, indexPayload domain.IndexPayloadFunc) error {
	if indexPayload == nil {
		return nil
	}
	payload, err := indexPayload(b)
	if err != nil {
		return fmt.Errorf("build index payload: %w", err)
	}
	return enqueueIndexMessage(ctx, tx, indexObjectTypeBrief, b.ID, payload)
}

// indexObjectTypeBrief mirrors indexer.ObjectTypeBrief. Duplicated rather than imported: the
// postgres package must not depend on the indexer, and the value is pinned by a test.
const indexObjectTypeBrief = "campaign_brief"

// ArchiveBrief soft-archives a brief.
func (r *BriefRepo) ArchiveBrief(ctx context.Context, projectID, id string, by *model.Actor, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	// Archive + outbox enqueue in ONE transaction. Archiving is TERMINAL: there is no "next
	// write" to repair the index, so if the post-commit publish is dropped the brief stays
	// searchable forever. The outbox row co-commits with the archive, so the relay can deliver
	// it even if this process dies immediately after the commit.
	archivedBy, err := marshalActor(by)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("archive brief: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// RETURNING the updated row makes the archive and the read of its result ONE statement, so
	// the caller indexes exactly what was committed. A separate read-then-archive would race a
	// concurrent ReplaceBrief/Approve and publish a snapshot that never existed in the table.
	// Archiving is the write MOST worth attributing — it is the one that removes a brief
	// from every list and cannot be undone through the API — so it takes an actor even
	// though it changes no content.
	b, err := scanBrief(tx.QueryRow(ctx, archiveBriefQuery, id, projectID, archivedBy))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row updated: absent, cross-project, or ALREADY archived (the status guard).
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("archive brief: %w", err)
	}

	if eerr := enqueueBriefIndex(ctx, tx, b, indexPayload); eerr != nil {
		return nil, fmt.Errorf("archive brief: %w", eerr)
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("archive brief: commit: %w", cerr)
	}
	return b, nil
}

func scanBrief(row pgx.Row) (*model.CampaignBrief, error) {
	var (
		b                                model.CampaignBrief
		programType, status              string
		url                              *string
		approvedBy, createdBy, updatedBy []byte
	)
	if err := row.Scan(
		&b.ID, &b.ProjectID, &programType, &b.EventSlug, &url,
		&b.Platforms, &b.EventDetails, &b.Copy, &b.Keywords, &b.Targeting,
		&status, &b.Version, &approvedBy, &b.ApprovedAt, &createdBy, &updatedBy,
		&b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return nil, err
	}
	b.ProgramType = model.ProgramType(programType)
	b.Status = model.BriefStatus(status)
	if url != nil {
		b.URL = *url
	}
	// Surface corrupt actor JSON rather than silently returning a nil audit
	// trail (which would hide data corruption until a downstream nil deref).
	ab, err := unmarshalActor(approvedBy)
	if err != nil {
		return nil, fmt.Errorf("scan brief: unmarshal approved_by: %w", err)
	}
	b.ApprovedBy = ab
	if b.CreatedBy, err = unmarshalActor(createdBy); err != nil {
		return nil, fmt.Errorf("scan brief: unmarshal created_by: %w", err)
	}
	if b.UpdatedBy, err = unmarshalActor(updatedBy); err != nil {
		return nil, fmt.Errorf("scan brief: unmarshal updated_by: %w", err)
	}
	return &b, nil
}

// nullJSON returns nil for empty raw JSON so the column stores SQL NULL.
func nullJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return []byte(j)
}

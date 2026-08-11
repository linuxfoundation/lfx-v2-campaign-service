// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// connectionCommonCols are the shared columns every provider table selects, in
// the fixed order scanConnection expects. Defined once so Get/Create/Update
// can't drift out of alignment with the scan.
//
// id is cast to text: it is a UUID column, and pgx/v5's binary codec cannot
// scan a uuid directly into a Go string — the ::text cast makes the scan into
// model.Connection's string fields work without changing the domain type.
// project_id is a TEXT column (it holds a project UUID *or* slug, e.g. "cncf"),
// so it scans into a string directly with no cast.
var connectionCommonCols = []string{
	"id::text", "project_id", "label", "account_id", "credentials",
	"status", "version", "created_by", "updated_by", "created_at", "updated_at",
}

// connectionSelectCols returns the full column list (common + provider-specific)
// for a provider, in scan order. Single source of truth for the SELECT/RETURNING
// column set across Get, Create, and Update.
func connectionSelectCols(provider model.Provider) []string {
	cfg := provider.ConfigKeys()
	cols := make([]string, 0, len(connectionCommonCols)+len(cfg))
	cols = append(cols, connectionCommonCols...)
	cols = append(cols, cfg...)
	return cols
}

// ConnectionRepo is a pgx-backed implementation of domain.ConnectionRepository.
// It works across every provider table by keying on provider.Table() and the
// provider's config-column allowlist.
type ConnectionRepo struct {
	db *Pool
}

// NewConnectionRepo returns a ConnectionRepo backed by pool.
func NewConnectionRepo(pool *Pool) *ConnectionRepo {
	return &ConnectionRepo{db: pool}
}

var _ domain.ConnectionRepository = (*ConnectionRepo)(nil)

// Get returns the project's connection for the provider (excluding soft-deleted
// rows), or domain.ErrNotFound.
func (r *ConnectionRepo) Get(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error) {
	if !provider.Valid() {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	cfgCols := provider.ConfigKeys()
	cols := connectionSelectCols(provider)

	//nolint:gosec // table and column names come from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"SELECT %s FROM %s WHERE project_id = $1 AND status <> 'deleted'",
		strings.Join(cols, ", "), provider.Table(),
	)
	row := r.db.QueryRow(ctx, q, projectID)
	c, err := scanConnection(row, provider, cfgCols)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get connection: %w", err)
	}
	return c, nil
}

// Disconnected reports whether the project once had a connection for this provider and
// explicitly removed it. Delete soft-deletes, and Get filters `status = 'deleted'` out, so
// without this probe a deliberate disconnect is indistinguishable from never having connected —
// and the dispatch fallback treats the latter as licence to use the LF-owned ad account.
//
// EXISTS rather than a row fetch: the caller needs one bit, and a project may accumulate several
// tombstones (the partial unique index constrains only the live row).
func (r *ConnectionRepo) Disconnected(ctx context.Context, projectID string, provider model.Provider) (bool, error) {
	if !provider.Valid() {
		return false, fmt.Errorf("unknown provider %q", provider)
	}
	//nolint:gosec // table name comes from a fixed internal allowlist, not user input.
	q := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s WHERE project_id = $1 AND status = 'deleted')", provider.Table())
	var exists bool
	if err := r.db.QueryRow(ctx, q, projectID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check disconnected connection: %w", err)
	}
	return exists, nil
}

// Create inserts the project's connection. Returns domain.ErrConflict if one
// already exists (partial unique index on project_id WHERE status <> 'deleted').
func (r *ConnectionRepo) Create(ctx context.Context, c *model.Connection) (*model.Connection, error) {
	if !c.Provider.Valid() {
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
	cfgCols := c.Provider.ConfigKeys()

	insertCols := append([]string{"project_id", "label", "account_id", "credentials", "created_by", "updated_by"}, cfgCols...)
	placeholders := make([]string, len(insertCols))
	args := make([]any, 0, len(insertCols))
	createdBy, err := marshalActor(c.CreatedBy)
	if err != nil {
		return nil, err
	}
	base := []any{c.ProjectID, nullStr(c.Label), c.AccountID, c.EncryptedCredentials, createdBy, createdBy}
	args = append(args, base...)
	for _, col := range cfgCols {
		args = append(args, nullStr(c.ProviderConfig[col]))
	}
	for i := range insertCols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}

	retCols := connectionSelectCols(c.Provider)

	//nolint:gosec // table and column names come from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) RETURNING %s",
		c.Provider.Table(), strings.Join(insertCols, ", "), strings.Join(placeholders, ", "), strings.Join(retCols, ", "),
	)
	row := r.db.QueryRow(ctx, q, args...)
	created, err := scanConnection(row, c.Provider, cfgCols)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ErrConflict
		}
		return nil, fmt.Errorf("create connection: %w", err)
	}
	return created, nil
}

// Update replaces config columns, gating on expectedVersion and bumping it.
// Credentials are untouched. Returns ErrNotFound / ErrPreconditionFailed.
func (r *ConnectionRepo) Update(ctx context.Context, c *model.Connection, expectedVersion int64) (*model.Connection, error) {
	return r.update(ctx, c, nil, expectedVersion)
}

// UpdateWithCredential is Update with the credential blob written by the SAME statement,
// so the row never holds one write's account beside the other's credential and a
// concurrent writer loses on the version check instead of interleaving.
func (r *ConnectionRepo) UpdateWithCredential(ctx context.Context, c *model.Connection, ciphertext []byte, expectedVersion int64) (*model.Connection, error) {
	if len(ciphertext) == 0 {
		// Writing an empty blob would leave a row that looks configured and cannot
		// authenticate. Update is the call for "config only".
		return nil, fmt.Errorf("update connection: credential ciphertext is required")
	}
	return r.update(ctx, c, ciphertext, expectedVersion)
}

// update is the one UPDATE both spellings issue; ciphertext nil leaves credentials alone.
func (r *ConnectionRepo) update(ctx context.Context, c *model.Connection, ciphertext []byte, expectedVersion int64) (*model.Connection, error) {
	if !c.Provider.Valid() {
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
	cfgCols := c.Provider.ConfigKeys()

	sets := []string{"label = $1", "account_id = $2", "updated_by = $3", "version = version + 1", "updated_at = now()"}
	updatedBy, err := marshalActor(c.UpdatedBy)
	if err != nil {
		return nil, err
	}
	args := []any{nullStr(c.Label), c.AccountID, updatedBy}
	if ciphertext != nil {
		args = append(args, ciphertext)
		sets = append(sets, fmt.Sprintf("credentials = $%d", len(args)))
	}
	for _, col := range cfgCols {
		args = append(args, nullStr(c.ProviderConfig[col]))
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	// WHERE params.
	args = append(args, c.ProjectID, expectedVersion)
	projPos, verPos := len(args)-1, len(args)

	// RETURNING, not a follow-up Get: this is an optimistic-concurrency write, so the row
	// it hands back is also the ETag the caller publishes. A separate re-read can observe a
	// LATER writer's row — the version check only proves nobody won the race BEFORE us, not
	// that nobody wins it after — and the caller would then publish an ETag for a state its
	// own write did not produce, silently making the next If-Match succeed against a version
	// it never saw. One statement makes the write and the returned snapshot the same event.
	//nolint:gosec // table and column names come from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"UPDATE %s SET %s WHERE project_id = $%d AND version = $%d AND status <> 'deleted' RETURNING %s",
		c.Provider.Table(), strings.Join(sets, ", "), projPos, verPos,
		strings.Join(connectionSelectCols(c.Provider), ", "),
	)
	updated, err := scanConnection(r.db.QueryRow(ctx, q, args...), c.Provider, cfgCols)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("update connection: %w", err)
		}
		// No row matched. Distinguish missing from stale version. Surface a transient Get
		// error rather than masking it as a precondition failure (which would make the
		// caller retry with a fresh ETag instead of backing off on a server error).
		_, gerr := r.Get(ctx, c.ProjectID, c.Provider)
		switch {
		case errors.Is(gerr, domain.ErrNotFound):
			return nil, domain.ErrNotFound
		case gerr != nil:
			return nil, gerr
		default:
			return nil, domain.ErrPreconditionFailed
		}
	}
	return updated, nil
}

// SetCredential replaces only the encrypted credential blob and bumps version,
// returning the updated connection so the handler can emit the new ETag.
func (r *ConnectionRepo) SetCredential(ctx context.Context, projectID string, provider model.Provider, ciphertext []byte, by *model.Actor) (*model.Connection, error) {
	if !provider.Valid() {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	updatedBy, err := marshalActor(by)
	if err != nil {
		return nil, err
	}
	// RETURNING for the reason spelled out on update: the returned row becomes the caller's
	// ETag, and this write is not even version-gated, so a re-read is MORE exposed here — any
	// concurrent writer at all can land between the two statements.
	//nolint:gosec // table and column names come from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"UPDATE %s SET credentials = $1, updated_by = $2, version = version + 1, updated_at = now() "+
			"WHERE project_id = $3 AND status <> 'deleted' RETURNING %s",
		provider.Table(), strings.Join(connectionSelectCols(provider), ", "),
	)
	updated, err := scanConnection(r.db.QueryRow(ctx, q, ciphertext, updatedBy, projectID), provider, provider.ConfigKeys())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("set credential: %w", err)
	}
	return updated, nil
}

// Delete soft-deletes the connection.
func (r *ConnectionRepo) Delete(ctx context.Context, projectID string, provider model.Provider, by *model.Actor) error {
	if !provider.Valid() {
		return fmt.Errorf("unknown provider %q", provider)
	}
	updatedBy, err := marshalActor(by)
	if err != nil {
		return err
	}
	//nolint:gosec // table name comes from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"UPDATE %s SET status = 'deleted', updated_by = $1, version = version + 1, updated_at = now() "+
			"WHERE project_id = $2 AND status <> 'deleted'",
		provider.Table(),
	)
	tag, err := r.db.Exec(ctx, q, updatedBy, projectID)
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// scanConnection scans a row with the fixed common columns followed by the
// provider's config columns (as *string) into a model.Connection.
func scanConnection(row pgx.Row, provider model.Provider, cfgCols []string) (*model.Connection, error) {
	var (
		c                    model.Connection
		label                *string
		createdBy, updatedBy []byte
	)
	cfgVals := make([]*string, len(cfgCols))
	dest := []any{
		&c.ID, &c.ProjectID, &label, &c.AccountID, &c.EncryptedCredentials,
		&c.Status, &c.Version, &createdBy, &updatedBy, &c.CreatedAt, &c.UpdatedAt,
	}
	for i := range cfgVals {
		dest = append(dest, &cfgVals[i])
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	c.Provider = provider
	if label != nil {
		c.Label = *label
	}
	// Surface corrupt actor JSON rather than silently returning a nil audit
	// trail, which would hide data corruption until a downstream nil deref.
	cb, err := unmarshalActor(createdBy)
	if err != nil {
		return nil, fmt.Errorf("scan connection: unmarshal created_by: %w", err)
	}
	c.CreatedBy = cb
	ub, err := unmarshalActor(updatedBy)
	if err != nil {
		return nil, fmt.Errorf("scan connection: unmarshal updated_by: %w", err)
	}
	c.UpdatedBy = ub
	c.ProviderConfig = make(map[string]string, len(cfgCols))
	for i, col := range cfgCols {
		if cfgVals[i] != nil {
			c.ProviderConfig[col] = *cfgVals[i]
		}
	}
	return &c, nil
}

func marshalActor(a *model.Actor) ([]byte, error) {
	if a == nil {
		return nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("marshal actor: %w", err)
	}
	return b, nil
}

func unmarshalActor(b []byte) (*model.Actor, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var a model.Actor
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
//
// It does not say WHICH constraint fired. Use it only where the statement can
// violate exactly one, or where every one it can violate maps to the same
// outcome. Where a 23505 is translated into a sentinel that means something
// specific, use isUniqueViolationOn: the SQLSTATE-only match silently adopts
// the next unique index added to the table.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// isUniqueViolationOn reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) raised by the named constraint or index.
//
// Postgres reports the constraint name in the error's CONSTRAINT field, so the
// caller does not have to reason about which unique indexes the table happens
// to carry today — a fact that changes with every migration, and that no test
// on the calling package would notice changing.
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == constraint
	}
	return false
}

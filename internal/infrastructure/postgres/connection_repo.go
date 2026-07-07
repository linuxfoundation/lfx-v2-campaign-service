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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// providerConfigColumns lists the provider-specific config columns for each
// provider table, in a fixed order. These names are compile-time constants
// (never user input), so building SQL from them is safe.
var providerConfigColumns = map[model.Provider][]string{
	model.ProviderGoogleAds:    {"login_customer_id"},
	model.ProviderLinkedInAds:  {"org_id"},
	model.ProviderMetaAds:      {"page_id", "app_id"},
	model.ProviderRedditAds:    {},
	model.ProviderTwitterAds:   {"funding_instrument_id"},
	model.ProviderMicrosoftAds: {"customer_id"},
	model.ProviderHubSpot:      {"portal_id", "sender_email", "sender_name", "brand_kit"},
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
	cfgCols := providerConfigColumns[provider]
	cols := append([]string{
		"id", "project_id", "label", "account_id", "credentials",
		"status", "version", "created_by", "updated_by", "created_at", "updated_at",
	}, cfgCols...)

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

// Create inserts the project's connection. Returns domain.ErrConflict if one
// already exists (UNIQUE(project_id) violation).
func (r *ConnectionRepo) Create(ctx context.Context, c *model.Connection) (*model.Connection, error) {
	if !c.Provider.Valid() {
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
	cfgCols := providerConfigColumns[c.Provider]

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

	retCols := append([]string{
		"id", "project_id", "label", "account_id", "credentials",
		"status", "version", "created_by", "updated_by", "created_at", "updated_at",
	}, cfgCols...)

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
	if !c.Provider.Valid() {
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
	cfgCols := providerConfigColumns[c.Provider]

	sets := []string{"label = $1", "account_id = $2", "updated_by = $3", "version = version + 1", "updated_at = now()"}
	updatedBy, err := marshalActor(c.UpdatedBy)
	if err != nil {
		return nil, err
	}
	args := []any{nullStr(c.Label), c.AccountID, updatedBy}
	for _, col := range cfgCols {
		args = append(args, nullStr(c.ProviderConfig[col]))
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	// WHERE params.
	args = append(args, c.ProjectID, expectedVersion)
	projPos, verPos := len(args)-1, len(args)

	//nolint:gosec // table and column names come from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"UPDATE %s SET %s WHERE project_id = $%d AND version = $%d AND status <> 'deleted'",
		c.Provider.Table(), strings.Join(sets, ", "), projPos, verPos,
	)
	tag, err := r.db.Exec(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("update connection: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish missing from stale version.
		if _, gerr := r.Get(ctx, c.ProjectID, c.Provider); errors.Is(gerr, domain.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, domain.ErrPreconditionFailed
	}
	return r.Get(ctx, c.ProjectID, c.Provider)
}

// SetCredential replaces only the encrypted credential blob and bumps version.
func (r *ConnectionRepo) SetCredential(ctx context.Context, projectID string, provider model.Provider, ciphertext []byte, by *model.Actor) error {
	if !provider.Valid() {
		return fmt.Errorf("unknown provider %q", provider)
	}
	updatedBy, err := marshalActor(by)
	if err != nil {
		return err
	}
	//nolint:gosec // table name comes from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"UPDATE %s SET credentials = $1, updated_by = $2, version = version + 1, updated_at = now() "+
			"WHERE project_id = $3 AND status <> 'deleted'",
		provider.Table(),
	)
	tag, err := r.db.Exec(ctx, q, ciphertext, updatedBy, projectID)
	if err != nil {
		return fmt.Errorf("set credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Delete soft-deletes the connection.
func (r *ConnectionRepo) Delete(ctx context.Context, projectID string, provider model.Provider) error {
	if !provider.Valid() {
		return fmt.Errorf("unknown provider %q", provider)
	}
	//nolint:gosec // table name comes from a fixed internal allowlist, not user input.
	q := fmt.Sprintf(
		"UPDATE %s SET status = 'deleted', version = version + 1, updated_at = now() "+
			"WHERE project_id = $1 AND status <> 'deleted'",
		provider.Table(),
	)
	tag, err := r.db.Exec(ctx, q, projectID)
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
	if a, err := unmarshalActor(createdBy); err == nil {
		c.CreatedBy = a
	}
	if a, err := unmarshalActor(updatedBy); err == nil {
		c.UpdatedBy = a
	}
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
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

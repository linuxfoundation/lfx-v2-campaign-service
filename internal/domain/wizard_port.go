// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// WizardSessionRepository persists email-creation wizard sessions.
//
// It is a SEPARATE port from BriefRepository even though a session is subordinate to a
// brief, for the same reason CreativeAssetRepository is: the wizard is an optional
// capability that a deployment can have entirely unwired (the service then answers its
// wizard routes with the typed 503 and every other brief route keeps working), and
// folding these methods into BriefRepository would make that partial state
// unrepresentable in the type system.
//
// There is no Delete. A session is the record of a run that may have created a real
// HubSpot draft, so removing it would destroy the only local trace of who asked for that
// draft and what it was generated from. See migration 000033's growth note.
type WizardSessionRepository interface {
	// CreateSession inserts a session and returns the stored row, including the
	// database-assigned id, version and timestamps.
	//
	// The row's (brief_id, project_id) pair is checked against campaign_briefs by a
	// composite foreign key, so a session can never be created for a brief the project
	// does not own; that violation surfaces as an error rather than ErrNotFound, because
	// the caller is expected to have read the brief first and reaching here with a bad
	// pair is a bug, not a client mistake.
	CreateSession(ctx context.Context, s *model.WizardSession) (*model.WizardSession, error)

	// GetSession returns one session scoped to its project AND brief, or ErrNotFound.
	//
	// Both scopes are required rather than looking up by id alone. The id is a UUID and
	// is not guessable, but "not guessable" is not an authorization model: every other
	// read in this service proves tenancy in the WHERE clause, and a session carries the
	// generated marketing copy for an unannounced event.
	GetSession(ctx context.Context, projectID, briefID, id string) (*model.WizardSession, error)

	// GetSessionByToken returns the session holding progressToken, or ErrNotFound.
	//
	// This is the ONE lookup that is not project-scoped, because the SSE handler is
	// given a token and nothing else — it resolves the session here and then checks the
	// caller's project against the row it found. Implementations must therefore treat an
	// empty token as ErrNotFound rather than matching rows whose token is NULL, or an
	// unauthenticated stream request with no token would resolve to an arbitrary
	// session.
	GetSessionByToken(ctx context.Context, progressToken string) (*model.WizardSession, error)

	// UpdateSession replaces a session's mutable fields, gating on expectedVersion.
	//
	// Returns ErrStaleWizardSession when the row exists at a different version and
	// ErrNotFound when it is gone; the two are distinguished inside one transaction,
	// because a caller told "not found" for what is really a stale write would retry
	// from scratch and create a second session for the same run.
	//
	// id, project_id, brief_id, created_by and created_at are NOT mutable: a session's
	// identity and provenance are fixed when it is created.
	UpdateSession(ctx context.Context, s *model.WizardSession, expectedVersion int64) (*model.WizardSession, error)
}

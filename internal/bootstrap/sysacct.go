// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package bootstrap installs the LF-owned system ad-account credentials that
// projects without a connection of their own fall back to.
//
// It exists because model.SystemProjectID is deliberately unreachable over HTTP
// (see rejectSystemScope in internal/service): every connection endpoint answers
// 404 there, so no request can install the row. That is the correct posture, and it
// is exactly why an out-of-band installer is REQUIRED rather than optional —
// without one the fallback can never fire and the feature ships turned off.
//
// The installer speaks to the repository and the encryptor directly, the same two
// ports the HTTP layer uses, so a row it writes is indistinguishable from one the
// API wrote: same encryption, same version counter, same audit fields.
package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// systemActor is stamped as created_by/updated_by on the system row. The reserved
// scope has no human owner and no bearer token behind it, so attributing the write
// to a person would be a lie; naming the installer is the honest answer and it is
// what an auditor reading updated_by needs in order to know which path wrote the row.
var systemActor = &model.Actor{Name: "system account bootstrap", Username: "sysacct-bootstrap"}

// InstallSystemCredentials installs or rotates the system account's credentials for
// one provider. It is IDEMPOTENT: run it twice with the same input and the second
// run rotates the credential onto the existing row rather than failing the singleton
// constraint, which is what makes it safe to wire into a deployment job.
//
// credsJSON is the plaintext credential document for the provider, exactly as the
// set-credential endpoint would receive it. It is encrypted here and the plaintext
// is never persisted or logged.
//
// accountID may be empty: that is the credentials-first bootstrap state, and the
// account-discovery endpoint exists to turn it into an account id. Requiring one
// here would mean knowing the customer id before holding the credential that could
// tell you what it is.
func InstallSystemCredentials(
	ctx context.Context,
	repo domain.ConnectionRepository,
	enc domain.Encryptor,
	provider model.Provider,
	accountID string,
	credsJSON []byte,
) error {
	if repo == nil || enc == nil {
		return errors.New("bootstrap: repository and encryptor are required")
	}
	if !provider.Valid() {
		return fmt.Errorf("bootstrap: %q is not a supported provider", provider)
	}
	// A credential document is validated as a non-empty JSON OBJECT, not merely as
	// valid JSON: `null`, `[]` and `"x"` all parse, and each would store a blob that
	// decrypts cleanly and then fails at dispatch time with nothing pointing back
	// here. The per-field requirements stay with the provider that knows them —
	// this only refuses what could never be right for any provider.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(credsJSON, &probe); err != nil {
		return fmt.Errorf("bootstrap: credentials must be a json object: %w", err)
	}
	if len(probe) == 0 {
		return errors.New("bootstrap: credentials json object is empty")
	}

	ct, err := enc.Encrypt(credsJSON)
	if err != nil {
		return fmt.Errorf("bootstrap: encrypt credentials: %w", err)
	}

	existing, gerr := repo.Get(ctx, model.SystemProjectID, provider)
	switch {
	case gerr == nil:
		// Rotation. Only SetCredential is used, deliberately: Update would also
		// rewrite the account id, so a rotation run that omitted -account-id would
		// silently CLEAR an account someone had already selected.
		if _, serr := repo.SetCredential(ctx, model.SystemProjectID, provider, ct, systemActor); serr != nil {
			return fmt.Errorf("bootstrap: rotate system %s credentials: %w", provider, serr)
		}
		if accountID != "" && accountID != existing.AccountID {
			upd := *existing
			upd.AccountID = accountID
			upd.UpdatedBy = systemActor
			// SetCredential above bumped the version, so the row's version is now
			// existing.Version+1. Passing the stale one would fail the optimistic
			// check and leave the credential rotated but the account id not.
			if _, uerr := repo.Update(ctx, &upd, existing.Version+1); uerr != nil {
				return fmt.Errorf("bootstrap: set system %s account id: %w", provider, uerr)
			}
		}
		return nil
	case errors.Is(gerr, domain.ErrNotFound):
		// First install. Not-found is the ONLY error that may create: any other
		// error means the row's state is unknown, and creating on top of an
		// existing-but-unreadable row is how you end up with two system accounts
		// or an overwritten credential nobody meant to replace.
		_, cerr := repo.Create(ctx, &model.Connection{
			ProjectID:            model.SystemProjectID,
			Provider:             provider,
			Label:                "Linux Foundation system account",
			AccountID:            accountID,
			EncryptedCredentials: ct,
			Status:               model.StatusActive,
			CreatedBy:            systemActor,
			UpdatedBy:            systemActor,
		})
		if cerr != nil {
			return fmt.Errorf("bootstrap: create system %s connection: %w", provider, cerr)
		}
		return nil
	default:
		return fmt.Errorf("bootstrap: read system %s connection: %w", provider, gerr)
	}
}

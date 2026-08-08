// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package bootstrap installs the LF-owned system ad-account credentials that projects without
// a connection of their own fall back to. model.SystemProjectID is unreachable over HTTP
// (rejectSystemScope), so an out-of-band installer is REQUIRED, not optional: without one the
// feature ships turned off. It writes through the repository and encryptor, the same two ports
// the HTTP layer uses, so its row is indistinguishable from an API-written one. Driven by the
// bootstrap-system-account subcommand; see docs/knowledge/code/internal-dispatch.md.
package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// systemActor is stamped as created_by/updated_by: the reserved scope has no bearer token.
var systemActor = &model.Actor{Name: "system account bootstrap", Username: "sysacct-bootstrap"}

// requiredCredentialKeys mirrors the Required() lists in design/connection.go, in the
// snake_case wire form: the contract is the documented REQUEST body, not an internal struct.
var requiredCredentialKeys = map[model.Provider][]string{
	model.ProviderGoogleAds:    {"refresh_token", "client_id", "client_secret", "developer_token"},
	model.ProviderLinkedInAds:  {"access_token"},
	model.ProviderMetaAds:      {"access_token", "app_secret"},
	model.ProviderRedditAds:    {"client_id", "client_secret", "refresh_token"},
	model.ProviderTwitterAds:   {"consumer_key", "consumer_secret", "access_token", "access_token_secret"},
	model.ProviderMicrosoftAds: {"client_id", "client_secret", "refresh_token", "developer_token"},
	model.ProviderHubSpot:      {"private_app_token"},
}

// credentialKey folds a field name to the form the READERS match on. Stored blobs and dispatch
// structs are both untagged, so encoding/json falls back to a case-insensitive match: `clientId`
// works, `client_id` cannot — and snake_case is what the API documents, so such a body encrypted
// cleanly, decoded to an all-zero struct and failed at dispatch, exit 0.
func credentialKey(k string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
}

// canonicalCredentials validates the document and folds its keys, refusing anything that is not
// a non-empty JSON OBJECT (`null`, `[]`, `"x"` all parse, none is a credential) or missing a
// required field, which would otherwise fail far away at dispatch.
func canonicalCredentials(provider model.Provider, credsJSON []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(credsJSON, &doc); err != nil {
		return nil, fmt.Errorf("bootstrap: credentials must be a json object: %w", err)
	}
	if len(doc) == 0 {
		return nil, errors.New("bootstrap: credentials json object is empty")
	}
	folded := make(map[string]json.RawMessage, len(doc))
	for k, v := range doc {
		// Two spellings of one field may differ; keeping whichever ranged last is a coin flip.
		if _, dup := folded[credentialKey(k)]; dup {
			return nil, fmt.Errorf("bootstrap: credentials contain two spellings of %q", credentialKey(k))
		}
		folded[credentialKey(k)] = v
	}
	var missing []string
	for _, want := range requiredCredentialKeys[provider] {
		if raw, ok := folded[credentialKey(want)]; !ok || len(raw) == 0 || string(raw) == `""` || string(raw) == "null" {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("bootstrap: %s credentials are missing %s", provider, strings.Join(missing, ", "))
	}
	return json.Marshal(folded)
}

// requiredConfigKeys are the non-secret ProviderConfig columns a dispatch adapter REFUSES to
// create a campaign without — the row is otherwise installable and dead. Others are optional.
var requiredConfigKeys = map[model.Provider][]string{
	model.ProviderLinkedInAds: {"org_id"},
	model.ProviderMetaAds:     {"page_id"},
	model.ProviderTwitterAds:  {"funding_instrument_id"},
}

// requireConfig checks the map about to be WRITTEN, not the flags as typed: on a rotation
// that is the merge below, so a key already on the row satisfies it.
func requireConfig(provider model.Provider, cfg map[string]string) error {
	for _, want := range requiredConfigKeys[provider] {
		if strings.TrimSpace(cfg[want]) == "" {
			return fmt.Errorf("bootstrap: %s requires -config %s", provider, want)
		}
	}
	return nil
}

// mergeConfig overlays the supplied flags on what the row already holds. Update rewrites EVERY
// config column, so replacing would NULL siblings a flag did not mention (Meta stores page_id and
// app_id, HubSpot four). nil means "write no config at all".
func mergeConfig(existing, supplied map[string]string) map[string]string {
	if len(supplied) == 0 {
		return nil
	}
	merged := make(map[string]string, len(existing)+len(supplied))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range supplied {
		merged[k] = v
	}
	return merged
}

// InstallSystemCredentials installs or rotates the system account's credentials for one
// provider. It is IDEMPOTENT — a second run rotates onto the existing row rather than failing
// the singleton constraint — which makes it safe in a deployment job. credsJSON is the plaintext
// document in the snake_case form set-credential documents; keys are folded before encryption
// and never logged. An empty accountID is the credentials-first state.
func InstallSystemCredentials(
	ctx context.Context,
	repo domain.ConnectionRepository,
	enc domain.Encryptor,
	provider model.Provider,
	accountID string,
	providerConfig map[string]string,
	credsJSON []byte,
) error {
	if repo == nil || enc == nil {
		return errors.New("bootstrap: repository and encryptor are required")
	}
	if !provider.Valid() {
		return fmt.Errorf("bootstrap: %q is not a supported provider", provider)
	}
	canonical, err := canonicalCredentials(provider, credsJSON)
	if err != nil {
		return err
	}

	ct, err := enc.Encrypt(canonical)
	if err != nil {
		return fmt.Errorf("bootstrap: encrypt credentials: %w", err)
	}

	existing, gerr := repo.Get(ctx, model.SystemProjectID, provider)
	switch {
	case gerr == nil:
		// Only SetCredential: Update would also rewrite the account id, so a rotation
		// omitting -account-id would CLEAR a selected account.
		if _, serr := repo.SetCredential(ctx, model.SystemProjectID, provider, ct, systemActor); serr != nil {
			return fmt.Errorf("bootstrap: rotate system %s credentials: %w", provider, serr)
		}
		// Config is rewritten only when supplied, same reason as the account id.
		cfg := mergeConfig(existing.ProviderConfig, providerConfig)
		if cfg != nil {
			if cerr := requireConfig(provider, cfg); cerr != nil {
				return cerr
			}
		}
		if idChanged := accountID != "" && accountID != existing.AccountID; idChanged || cfg != nil {
			upd := *existing
			if idChanged {
				upd.AccountID = accountID
			}
			if cfg != nil {
				upd.ProviderConfig = cfg
			}
			upd.UpdatedBy = systemActor
			// SetCredential bumped the version; the stale one fails the optimistic check.
			if _, uerr := repo.Update(ctx, &upd, existing.Version+1); uerr != nil {
				return fmt.Errorf("bootstrap: set system %s account id/config: %w", provider, uerr)
			}
		}
		return nil
	case errors.Is(gerr, domain.ErrNotFound):
		// Not-found is the ONLY error that may create: on any other the row's state is
		// unknown, and creating over it overwrites a credential nobody meant to replace.
		if verr := requireConfig(provider, providerConfig); verr != nil {
			return verr
		}
		_, cerr := repo.Create(ctx, &model.Connection{
			ProjectID:            model.SystemProjectID,
			Provider:             provider,
			Label:                "Linux Foundation system account",
			AccountID:            accountID,
			ProviderConfig:       providerConfig,
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

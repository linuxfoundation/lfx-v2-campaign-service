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
	"regexp"
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
		// Decoded as a STRING, not merely checked for presence: every dispatcher
		// unmarshals these fields into string struct members, so `"client_id": 123`
		// or `"  "` installs cleanly, exits 0, and fails at dispatch — the exact
		// deferred failure this validation exists to prevent.
		var v string
		if raw, ok := folded[credentialKey(want)]; !ok ||
			json.Unmarshal(raw, &v) != nil || strings.TrimSpace(v) == "" {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("bootstrap: %s credentials are missing %s", provider, strings.Join(missing, ", "))
	}
	return json.Marshal(folded)
}

var (
	numericID = regexp.MustCompile(`^[0-9]+$`)
	alnumID   = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

// valueShapes is the shape rule each non-secret id is ALREADY held to at the two places it
// is read, gathered where it is WRITTEN. This installer writes past the API straight to the
// repository, so without it a value the rest of the system refuses lands on an ACTIVE system
// row and fails every dispatch instead — with the failure surfacing far from the operator who
// typed it.
//
// Two sources, because the constraint lives in different places per provider, and mirroring
// only one of them was the bug: design/connection.go carries a Pattern() for LinkedIn, Meta
// and X, and for Google Ads, Microsoft and Reddit the constraint exists only as a RUNTIME
// validator (dispatch's storedCustomerIDRE, microsoft's accountIDRE, reddit's accountIDRe —
// the last two also guard header and path interpolation). Taking the design as the whole
// contract let `-provider google-ads -account-id foo` install cleanly and exit 0.
//
// A provider/key absent here is unconstrained at BOTH sources — HubSpot's list id, Meta's
// app_id — not merely absent from the design.
var valueShapes = map[model.Provider]map[string]*regexp.Regexp{
	// design/connection.go Pattern()
	model.ProviderLinkedInAds: {"account_id": numericID, "org_id": numericID},
	model.ProviderMetaAds:     {"account_id": regexp.MustCompile(`^act_[0-9]+$`), "page_id": numericID},
	model.ProviderTwitterAds:  {"account_id": alnumID, "funding_instrument_id": alnumID},
	// Runtime validators only — the design checks presence alone for these.
	model.ProviderGoogleAds:    {"account_id": numericID, "login_customer_id": numericID},
	model.ProviderMicrosoftAds: {"account_id": numericID, "customer_id": numericID},
	model.ProviderRedditAds:    {"account_id": regexp.MustCompile(`^[A-Za-z0-9_]+$`)},
}

// maxValueLen is design/connection.go's MaxLength(64). The runtime-only validators bound
// nothing, so applying it to them too is a tightening, not a mirror — 64 characters is far
// past any real numeric account id, and an unbounded value here reaches a header or a path.
const maxValueLen = 64

// requireShapes validates the values as SUPPLIED, and only those: an omitted account id is
// the legal credentials-first state, and a key not supplied on a rotation keeps whatever the
// row already holds — which was validated when it was written.
func requireShapes(provider model.Provider, accountID string, cfg map[string]string) error {
	supplied := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		supplied[k] = v
	}
	if accountID != "" {
		supplied["account_id"] = accountID
	}
	for key, re := range valueShapes[provider] {
		v, ok := supplied[key]
		if !ok {
			continue
		}
		if len(v) > maxValueLen || !re.MatchString(v) {
			return fmt.Errorf("bootstrap: %s %s %q does not match the shape this value is held to elsewhere (%s, at most %d chars) — see valueShapes",
				provider, key, v, re, maxValueLen)
		}
	}
	return nil
}

// requireKnownConfigKeys refuses a -config key the SELECTED provider does not persist.
//
// Storage has one column per key (model.Provider.ConfigKeys), so an unknown key is not stored
// somewhere unhelpful — it is DROPPED, and the command still exits 0. The operator is then told
// the setting is installed when nothing holds it, which is the whole failure mode: `-provider
// google-ads -config org_id=123` is LinkedIn's key, valid-looking and silently discarded.
//
// A per-provider check rather than a global key set, because most of these keys are real
// somewhere: the mistake this catches is using the right key on the wrong provider, which a
// union of every provider's keys would wave through.
func requireKnownConfigKeys(provider model.Provider, cfg map[string]string) error {
	known := make(map[string]bool, 4)
	for _, k := range provider.ConfigKeys() {
		known[k] = true
	}
	unknown := make([]string, 0, len(cfg))
	for k := range cfg {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	allowed := provider.ConfigKeys()
	if len(allowed) == 0 {
		return fmt.Errorf("bootstrap: %s stores no -config keys, but %s was supplied",
			provider, strings.Join(unknown, ", "))
	}
	return fmt.Errorf("bootstrap: %s does not store -config %s; it stores %s",
		provider, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
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

// accountDiscoveryProviders are the providers whose dispatcher can enumerate the accounts a
// credential reaches (domain.AccountLister, wired only for Google Ads today). They are the
// ONLY ones for which a credentials-first row is a real lifecycle state rather than a dead row.
//
// The distinction is not cosmetic. Every other adapter refuses an empty account id outright —
// internal/dispatch/{linkedin,meta,reddit,twitter,microsoft}.go each guard on it — and none of
// them offers an endpoint that would tell an operator what to put there, so an account-less
// LinkedIn or Meta system row is installable, reports success, and then fails every dispatch
// with no path to completion. That is exactly the failure requiredConfigKeys above exists to
// prevent, applied to the one column that is not part of ProviderConfig.
var accountDiscoveryProviders = map[model.Provider]bool{
	model.ProviderGoogleAds: true,
}

// requireAccountID checks the value about to be WRITTEN, for the same reason requireConfig
// does: on a rotation that omits -account-id the row keeps the id it already has, and that
// satisfies this.
func requireAccountID(provider model.Provider, effective string) error {
	if accountDiscoveryProviders[provider] || strings.TrimSpace(effective) != "" {
		return nil
	}
	return fmt.Errorf("bootstrap: %s requires -account-id: its dispatcher refuses a connection without one and "+
		"there is no account-discovery endpoint for this provider to finish the row later", provider)
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
// and never logged. An empty accountID is the credentials-first state, and it is accepted only
// for a provider with account discovery (see accountDiscoveryProviders) — for the rest it would
// install a row every dispatch refuses and nothing can complete.
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
	// Valid() is broader than what this row can ever be USED for. The reserved-scope
	// fallback in credsSource.systemConn is gated on Kind() == paid ads, deliberately:
	// the audience builder resolves HubSpot through the same function, and a fallback
	// there would write one project's contact lists into the LF's own portal. So a
	// HubSpot system row is installable, reports success, and is then reachable by
	// nothing — the same install-a-dead-row failure requireAccountID and
	// requireKnownConfigKeys exist to prevent, one level up. Refuse it here rather than
	// leave an operator holding a row they cannot use and cannot tell is unused.
	//
	// Asked as a classification, not as a name comparison, so a provider added later is
	// admitted only once it is classified — the same default-deny the fallback uses.
	if !provider.IsPaidAds() {
		return fmt.Errorf("bootstrap: %s is not a paid-ads provider, so a system-scope row for it "+
			"could never be used: the reserved-scope fallback resolves paid-ads providers only", provider)
	}
	if err := requireKnownConfigKeys(provider, providerConfig); err != nil {
		return err
	}
	if err := requireShapes(provider, accountID, providerConfig); err != nil {
		return err
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
		// ONE version-gated write. Update-then-SetCredential was two, and the order only
		// chose WHICH mixed state a failure left behind: this one could run the old
		// credential against the new account, the reverse the new credential against the
		// old account. Concurrent runs were worse — SetCredential is not version-gated, so
		// two rotations could finish with one run's account and the other's credential and
		// nothing would detect it. UpdateWithCredential writes account, config and
		// credential in a single statement gated on existing.Version, so a partial write is
		// not reachable and a concurrent writer loses the version check instead.
		//
		// Config and account id are rewritten only when supplied: the statement rewrites
		// every column, so a rotation omitting -account-id would otherwise CLEAR the
		// selection.
		cfg := mergeConfig(existing.ProviderConfig, providerConfig)
		if cfg != nil {
			if cerr := requireConfig(provider, cfg); cerr != nil {
				return cerr
			}
		}
		upd := *existing
		if accountID != "" {
			upd.AccountID = accountID
		}
		if aerr := requireAccountID(provider, upd.AccountID); aerr != nil {
			return aerr
		}
		if cfg != nil {
			upd.ProviderConfig = cfg
		}
		upd.UpdatedBy = systemActor
		if _, uerr := repo.UpdateWithCredential(ctx, &upd, ct, existing.Version); uerr != nil {
			if errors.Is(uerr, domain.ErrPreconditionFailed) {
				return fmt.Errorf("bootstrap: system %s connection changed while this command ran; nothing was written, rerun it: %w", provider, uerr)
			}
			return fmt.Errorf("bootstrap: rotate system %s connection: %w", provider, uerr)
		}
		return nil
	case errors.Is(gerr, domain.ErrNotFound):
		// Not-found is the ONLY error that may create: on any other the row's state is
		// unknown, and creating over it overwrites a credential nobody meant to replace.
		if verr := requireConfig(provider, providerConfig); verr != nil {
			return verr
		}
		if aerr := requireAccountID(provider, accountID); aerr != nil {
			return aerr
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

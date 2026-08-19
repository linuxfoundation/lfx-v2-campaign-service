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
	var missing, mistyped, padded []string
	for _, want := range requiredCredentialKeys[provider] {
		// Decoded as a STRING, not merely checked for presence: every dispatcher
		// unmarshals these fields into string struct members, so `"client_id": 123`
		// or `"  "` installs cleanly, exits 0, and fails at dispatch — the exact
		// deferred failure this validation exists to prevent.
		//
		// Omitted and present-but-not-a-string are reported SEPARATELY, matching
		// validateConditionalGroups below. Here the distinction is only about the
		// MESSAGE — every outcome in this loop is fatal, so there is no all-or-none
		// escape hatch for a uniform type fault to slip through — but "credentials are
		// missing client_id" for `"client_id": 123` sends an operator hunting for a
		// field they did supply, and the two faults are corrected differently.
		var v string
		raw, ok := folded[credentialKey(want)]
		switch {
		case !ok:
			missing = append(missing, want)
		case json.Unmarshal(raw, &v) != nil, strings.TrimSpace(v) == "":
			mistyped = append(mistyped, want)
		case v != strings.TrimSpace(v):
			// Surrounding whitespace is REFUSED, not trimmed away. Testing only the
			// trimmed value while encrypting the original was the same deferred failure
			// in a subtler form: `"access_token":" token "` passed, and LinkedIn's
			// preflight rejects a padded token (internal/platform/linkedin/client.go),
			// so the install exited 0 having written a system row every dispatch refuses.
			//
			// Refused rather than canonicalized because a credential is opaque to this
			// command: silently rewriting one would hide a truncated paste, and no
			// provider issues a secret whose surrounding whitespace is significant.
			padded = append(padded, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("bootstrap: %s credentials are missing %s", provider, strings.Join(missing, ", "))
	}
	if len(mistyped) > 0 {
		sort.Strings(mistyped)
		return nil, fmt.Errorf("bootstrap: %s credentials supply %s but not as a non-empty string; every dispatcher decodes these into string fields, so the row would install cleanly and fail at dispatch",
			provider, strings.Join(mistyped, ", "))
	}
	if len(padded) > 0 {
		sort.Strings(padded)
		return nil, fmt.Errorf("bootstrap: %s credentials have surrounding whitespace in %s; a secret is stored verbatim, so the padding would be sent to the provider",
			provider, strings.Join(padded, ", "))
	}
	if err := validateConditionalGroups(provider, folded); err != nil {
		return nil, err
	}
	return json.Marshal(folded)
}

// conditionalCredentialGroups are all-or-none field sets: supply every member or none. They
// are NOT in requiredCredentialKeys because each member is individually optional — it is the
// PARTIAL set that is invalid, which a required-key list cannot express.
var conditionalCredentialGroups = map[model.Provider][]string{
	// LinkedIn refresh material. LinkedIn issues refresh tokens only to approved Marketing
	// Developer Platform partners, so a bearer-only system row is legitimate and common;
	// what is never legitimate is one or two of the three.
	model.ProviderLinkedInAds: {"refresh_token", "client_id", "client_secret"},
}

// validateConditionalGroups enforces the all-or-none rule the API applies at
// validateLinkedInRefreshCredentials (internal/service/connection.go), which this installer
// bypasses entirely by writing past the API straight to the repository.
//
// It matters most precisely here. The system row is the fallback for every project that has
// connected no account of its own, so a partial paste degrades the highest-blast-radius row
// in the deployment: CanRefresh() returns false, the install exits 0, and the row silently
// behaves as bearer-only until the access token ages out ~60 days later — reappearing as the
// outage the refresh support was built to prevent, far from the operator who typed it.
func validateConditionalGroups(provider model.Provider, folded map[string]json.RawMessage) error {
	group, ok := conditionalCredentialGroups[provider]
	if !ok {
		return nil
	}
	var present, absent, padded, mistyped []string
	for _, want := range group {
		// Three outcomes, not two. A key that is genuinely OMITTED is absent, and absence
		// is legitimate here — a bearer-only LinkedIn row supplies none of the trio. A key
		// that is PRESENT but is not a non-empty JSON string is a TYPE FAULT, and folding
		// it into `absent` was the defect: when every member of the group is malformed the
		// absence is UNANIMOUS, `present` is empty, the all-or-none guard returns nil, and
		// the malformed blob is persisted for dispatch to fail on decoding into
		// linkedinCreds. The guard cannot see a uniform fault by construction, so the type
		// fault is collected separately and refused on its own terms.
		//
		// It is NOT folded into `present` either: that would report an all-or-none
		// violation ("supplied refresh_token but missing client_id") for what is actually
		// `"client_id": 123`, sending the operator to look for a field they did supply.
		var v string
		raw, found := folded[credentialKey(want)]
		if !found {
			absent = append(absent, want)
			continue
		}
		if json.Unmarshal(raw, &v) != nil {
			// Present and not a JSON string at all: a number, object, array or null.
			// Every dispatcher unmarshals these into string struct members, so this
			// blob decodes to an all-zero struct at dispatch, far from the operator.
			mistyped = append(mistyped, want)
			continue
		}
		if strings.TrimSpace(v) != "" {
			// Surrounding whitespace is REFUSED here for the same reason the required-key
			// loop refuses it, and this loop is where the refresh trio is reached at all:
			// requiredCredentialKeys[linkedin-ads] is {"access_token"} ONLY, so the padding
			// check above never sees refresh_token, client_id or client_secret. Without
			// this, `"client_id":" 123 "` installs cleanly on the SYSTEM row — the fallback
			// for every project with no connection of its own, the highest blast radius in
			// the deployment — satisfies CanRefresh() because that gates on the TRIMMED
			// value, and is then sent verbatim to LinkedIn's token endpoint, which rejects
			// it as invalid_client on every refresh until a human re-pastes it.
			if v != strings.TrimSpace(v) {
				padded = append(padded, want)
			}
			present = append(present, want)
			continue
		}
		// A supplied-but-blank string ("" or "   ") is a supplied key holding no
		// credential. It is the same type as the field it should be, so it is a value
		// fault rather than a type fault, but it is equally unusable: refusing it here
		// keeps a whitespace-only client_secret from satisfying the group.
		mistyped = append(mistyped, want)
	}
	if len(mistyped) > 0 {
		sort.Strings(mistyped)
		return fmt.Errorf("bootstrap: %s credentials supply %s but not as a non-empty string; every dispatcher decodes these into string fields, so the row would install cleanly and fail at dispatch",
			provider, strings.Join(mistyped, ", "))
	}
	if len(padded) > 0 {
		sort.Strings(padded)
		return fmt.Errorf("bootstrap: %s credentials have surrounding whitespace in %s; a secret is stored verbatim, so the padding would be sent to the provider",
			provider, strings.Join(padded, ", "))
	}
	if len(present) == 0 || len(absent) == 0 {
		return nil
	}
	sort.Strings(present)
	sort.Strings(absent)
	return fmt.Errorf("bootstrap: %s credentials are all-or-none for %s: supplied %s but missing %s; supply all of them, or none for a bearer-only connection",
		provider, strings.Join(group, ", "), strings.Join(present, ", "), strings.Join(absent, ", "))
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
// row already holds — which was validated when it was written. An empty value is a CLEAR
// (see mergeConfig) and is skipped for the same reason: there is no value to hold to a shape.
func requireShapes(provider model.Provider, accountID string, cfg map[string]string) error {
	supplied := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		if v == "" {
			continue
		}
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
	// Reddit joined this list when the conversion pixel moved onto the connection
	// (migration 000025). It meets the stated bar exactly: the Reddit client refuses EVERY
	// campaign create without a pixel -- not only the "conversions" objective its API docs
	// describe -- so a system row installed without one is installable and dead. Worse than
	// dead, in fact: the LF system row is the FALLBACK for every project that has connected
	// no Reddit account of its own, so one pixel-less install silently refuses paid creates
	// for all of them, with the failure surfacing per-project at dispatch rather than once
	// at install.
	model.ProviderRedditAds: {"conversion_pixel_id"},
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

// accountDiscoveryProviders are the providers for which a credentials-first row is a real
// lifecycle state rather than a dead row.
//
// The distinction is not cosmetic. The remaining adapters refuse an empty account id outright —
// internal/dispatch/{linkedin,reddit,twitter,microsoft}.go each guard on it — so an
// account-less system row for one of them is installable, reports success, and then fails
// every dispatch. What differs is whether the operator can RECOVER. Reddit and X have no
// discovery endpoint, so an account-less row is unrecoverable from inside this API. LinkedIn
// does have one — call discovery, rerun bootstrap with the chosen id — so its row is
// recoverable; what it lacks is DIAGNOSIS, because the create failure names nothing, leaving
// the operator with no reason to go looking. Microsoft is excluded by neither: it has both
// halves and its absence is sequencing alone (see the current state below). That is exactly the failure requiredConfigKeys
// above exists to prevent, applied to the one column that is not part of ProviderConfig.
//
// **Membership is NOT "the dispatcher implements the service-side AccountLister".** (The
// interface lives in internal/service/orchestrator.go; internal/domain owns only the
// ErrAccountsUnsupported sentinel it is paired with.) Discovery is only half of a completable
// lifecycle: the other half is that the path which DOES need an account id fails in a way that
// names the missing choice, so the operator is told what to go and use the discovery endpoint
// for. Meta had the first half from LFXV2-3062 and was deliberately excluded here until it had
// the second; LFXV2-3061 added it — internal/dispatch/meta.go's requireMetaAccountID tags an
// empty id with domain.ErrAccountNotSelected, which unusableConnectionReason reports as
// "account_not_selected". (Only Dispatch needs it: Meta's ToggleStatus and ReadMetrics target
// the campaign node by id and document that they need no account id, so there is nothing to tag
// there.) Both halves are present, so an account-less Meta system row is now a completable
// state rather than a dead one.
//
// Be exact about WHERE the naming lands, because Meta's only account-needing path is the
// asynchronous one and that changes the answer. dispatchPlatform collapses every dispatcher
// error into the same "platform campaign creation failed" job result, so the reason token
// reaches the operator in the dispatch-failure LOG LINE, not by polling the job. Google Ads
// is identical on its create path; what differs is that Google Ads' toggle and metrics need
// the account id too and answer a synchronous 409, and Meta's do not. Log-only is still the
// second half — an unclassified error names nothing at all — but it is a weaker signal than
// the Google Ads case, and someone weighing the next provider should weigh the real one.
//
// The bar for adding a provider here is those two halves together, not either alone.
//
// CURRENT STATE after LFXV2-3064, which added the LinkedIn and Microsoft discovery endpoints —
// the four excluded providers are no longer excluded for the same reason, and the difference is
// what tells you how far each is from qualifying:
//
//   - Reddit and X lack the FIRST half. Neither platform client has a ListAdAccounts, so no
//     discovery endpoint exists and nothing inside this API could tell an operator what to put
//     in the account id.
//   - LinkedIn has discovery but lacks the SECOND. resolveLinkedInCredentials tags
//     ErrAccountNotSelected, but LinkedInDispatcher.Dispatch does not call it — it validates
//     inline and answers an empty account id with a bare notCreated, so the create path names
//     nothing. Routing Dispatch through the shared resolver is what would earn it a place.
//   - Microsoft has BOTH halves. validateMicrosoftConnection is called by Dispatch itself and
//     tags the missing choice, and discovery landed with this ticket. Its absence from this map
//     is therefore a SEQUENCING decision, not a missing capability — adding it changes what this
//     CLI accepts and belongs in its own commit rather than riding along with the endpoints.
//     Note this map is a DIFFERENT gate from design/connection.go's Required("account_id"),
//     which governs the public connection APIs; a provider can be eligible for one and not yet
//     admitted to the other, which is exactly Microsoft's position.
//
// Stating which half is missing matters because the halves are earned separately, and an
// enumeration of members goes stale silently — this comment described a Google/Meta-only world
// for two tickets after that stopped being true.
var accountDiscoveryProviders = map[model.Provider]bool{
	model.ProviderGoogleAds: true,
	model.ProviderMetaAds:   true,
}

// requireAccountID checks the value about to be WRITTEN, for the same reason requireConfig
// does: on a rotation that omits -account-id the row keeps the id it already has, and that
// satisfies this.
func requireAccountID(provider model.Provider, effective string) error {
	if accountDiscoveryProviders[provider] || strings.TrimSpace(effective) != "" {
		return nil
	}
	return fmt.Errorf("bootstrap: %s requires -account-id: its dispatcher refuses a connection without one and "+
		"this provider has no credentials-first bootstrap to finish the row later", provider)
}

// mergeConfig overlays the supplied flags on what the row already holds. Update rewrites EVERY
// config column, so replacing would NULL siblings a flag did not mention (Meta stores page_id and
// app_id, HubSpot four). nil means "write no config at all".
//
// An empty supplied value is the explicit CLEAR, and it is why the merge needs three states
// rather than two: not mentioned keeps, `k=v` sets, `k=` removes. Preserve-by-default is right —
// a rotation should not have to restate every column — but on its own it makes an obsolete
// optional column permanent, and this scope has no other writer to remove it (rejectSystemScope
// blocks HTTP). A merge that clears the last remaining key returns an EMPTY, non-nil map, which
// the caller writes: clearing the only column has to reach storage like any other change.
func mergeConfig(existing, supplied map[string]string) map[string]string {
	if len(supplied) == 0 {
		return nil
	}
	merged := make(map[string]string, len(existing)+len(supplied))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range supplied {
		if v == "" {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	return merged
}

// clearedKeys lists the config keys a run asked to REMOVE, sorted so the message is stable.
func clearedKeys(cfg map[string]string) []string {
	cleared := make([]string, 0, len(cfg))
	for k, v := range cfg {
		if v == "" {
			cleared = append(cleared, k)
		}
	}
	sort.Strings(cleared)
	return cleared
}

// InstallSystemCredentials installs or rotates the system account's credentials for one
// provider. It is IDEMPOTENT — a second run rotates onto the existing row rather than failing
// the singleton constraint — which makes it safe in a deployment job. credsJSON is the plaintext
// document in the snake_case form set-credential documents; keys are folded before encryption
// and never logged.
//
// accountID and providerConfig are TRI-STATE, and which state an omission means depends on
// whether a row is already there — the distinction this signature exists to make. On a first
// install an omitted accountID is the credentials-first state, accepted only for a provider with
// account discovery (see accountDiscoveryProviders); for the rest it would install a row every
// dispatch refuses and nothing can complete. On a ROTATION an omission means keep, so a run that
// rotates onto a credential for a DIFFERENT account and says nothing about -account-id would
// otherwise dispatch the new credential at the old account — silently, and with both values
// individually valid. clearAccountID and an empty config value are how a caller says remove
// instead of keep; a clear is meaningless before the row exists and is refused there.
func InstallSystemCredentials(
	ctx context.Context,
	repo domain.ConnectionRepository,
	enc domain.Encryptor,
	provider model.Provider,
	accountID string,
	clearAccountID bool,
	providerConfig map[string]string,
	credsJSON []byte,
) error {
	if repo == nil || enc == nil {
		return errors.New("bootstrap: repository and encryptor are required")
	}
	if clearAccountID && accountID != "" {
		return fmt.Errorf("bootstrap: -clear-account-id and -account-id %q ask for opposite things; supply one", accountID)
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
		// Validate the EFFECTIVE config, not only a supplied one. mergeConfig returns nil
		// when -config is omitted, so gating this on `cfg != nil` skipped validation on
		// exactly the rotation that needs it: a row written BEFORE a key joined
		// requiredConfigKeys (a pre-000025 Reddit row with no conversion_pixel_id) could
		// take fresh credentials, report success, and remain unusable — and because the LF
		// system row is the fallback for every project with no Reddit connection of its
		// own, unusable for all of them, surfacing per-project at dispatch instead of once
		// here. requireAccountID below already validates the effective value
		// unconditionally; this now matches it.
		effective := cfg
		if effective == nil {
			effective = existing.ProviderConfig
		}
		if cerr := requireConfig(provider, effective); cerr != nil {
			return cerr
		}
		upd := *existing
		switch {
		case accountID != "":
			upd.AccountID = accountID
		case clearAccountID:
			upd.AccountID = ""
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
		//
		// A clear is refused rather than treated as a no-op. There is nothing to remove, so
		// obeying it and obeying its opposite produce the same row — which means accepting it
		// would report success for an instruction that was not carried out, and the likely
		// cause is that the operator believed they were rotating a row that is not there.
		if clearAccountID {
			return fmt.Errorf("bootstrap: -clear-account-id has nothing to clear: no system %s connection exists yet", provider)
		}
		if cleared := clearedKeys(providerConfig); len(cleared) > 0 {
			return fmt.Errorf("bootstrap: -config %s asks to clear a column, but no system %s connection exists yet",
				strings.Join(cleared, ", "), provider)
		}
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

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package dispatch holds the per-platform PlatformDispatcher adapters that bridge the
// orchestrator to the ad-platform API clients. Each adapter fetches + decrypts the
// project's connection for its provider, maps the brief + per-platform config onto
// the client's create input, calls the client, and maps the result back to a
// model.Campaign. The orchestrator is agnostic to the platforms; this package is the
// only place that knows both the orchestrator's contract and the platform clients.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// campaignDateLayout is the wire format for the per-platform config start/end dates
// (YYYY-MM-DD), documented in docs/api-catalog.md for every platform config.
const campaignDateLayout = "2006-01-02"

// maxPersistedBudget is the largest value the campaigns.budget_amount column can hold
// (NUMERIC(14,2) → 12 integer digits, i.e. < 10^12). Some platform clients (Meta,
// Twitter) accept a larger budget than this — for those the campaign can be created
// upstream and only THEN would the row write fail with a numeric overflow. To avoid
// losing the record of a created campaign, applyCampaignConfig leaves budget_amount
// NULL (and logs) rather than persisting an over-range value.
const maxPersistedBudget = 1e12 - 0.01

// applyCampaignConfig populates the persistence-contract columns on c that only the
// per-platform config knows: budget_amount, budget_type, start_date, end_date, and
// config_snapshot (docs/architecture.md — the campaigns row stores these). Without it
// every dispatched row would have NULL budget/schedule/config despite those values
// being used upstream. Shared by all adapters so the persisted contract is identical
// across platforms.
//
//   - budget: whole units in the platform's budget currency (0 → left NULL / unset).
//   - lifetime: true → BudgetLifetime, false → BudgetDaily (only set when budget > 0).
//   - start/end: YYYY-MM-DD strings; a blank or unparseable value is left NULL (the
//     client already validated dates on the create path, so this is defensive).
//   - snapshot: the validated per-platform config struct; marshaled into
//     ConfigSnapshot. A marshal failure is logged (not fatal) and leaves it NULL.
func applyCampaignConfig(ctx context.Context, c *model.Campaign, budget float64, lifetime bool, startDate, endDate string, snapshot any) {
	if budget > 0 {
		if budget > maxPersistedBudget {
			// The campaign exists upstream (some clients accept a larger budget than the
			// budget_amount column holds); persisting the over-range value would fail the
			// whole row write with a numeric overflow and lose the record. Leave it NULL
			// and log so the row still persists (id/status/config) for reconciliation.
			slog.WarnContext(ctx, "campaign budget exceeds the persistable range; budget_amount left empty",
				"platform", string(c.Platform), "budget", budget, "max", maxPersistedBudget)
		} else {
			b := budget
			c.BudgetAmount = &b
			bt := model.BudgetDaily
			if lifetime {
				bt = model.BudgetLifetime
			}
			c.BudgetType = &bt
		}
	}
	c.StartDate = parseCampaignDate(startDate)
	c.EndDate = parseCampaignDate(endDate)
	if snapshot != nil {
		if raw, err := json.Marshal(snapshot); err != nil {
			slog.WarnContext(ctx, "failed to marshal campaign config snapshot (ConfigSnapshot left empty)",
				"platform", string(c.Platform), "error", err)
		} else {
			c.ConfigSnapshot = raw
		}
	}
}

// sanitizeSnapshotURL strips the query and fragment from a URL before it is stored in
// config_snapshot (which is persisted UNENCRYPTED). A destination/post URL's query or
// fragment can carry secrets, so the snapshot keeps only scheme+host+path. An absolute
// URL is reduced to that; a value that does not parse as an absolute URL (or carries
// userinfo/credentials) is truncated at the first '?'/'#' and dropped entirely if it
// still contains a credential delimiter '@', mirroring the reddit client's redactURL
// fail-closed behavior. An empty input stays empty.
func sanitizeSnapshotURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if u, err := url.Parse(trimmed); err == nil && u.IsAbs() && u.Host != "" && u.User == nil {
		redacted := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		return redacted.String()
	}
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if strings.Contains(trimmed, "@") {
		return "" // fail closed: don't store a value that may embed userinfo credentials
	}
	return trimmed
}

// parseCampaignDate parses a YYYY-MM-DD config date to a *time.Time (UTC), returning
// nil for a blank or unparseable value (the column is nullable).
func parseCampaignDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.Parse(campaignDateLayout, s)
	if err != nil {
		return nil
	}
	return &t
}

// connReader is the read side of the connection repository the adapters need. Kept
// to the single method they use so a test can supply a tiny fake.
type connReader interface {
	Get(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error)
	// Disconnected reports whether the project once had a connection for this provider and
	// explicitly removed it. It exists because Get cannot answer it: Delete SOFT-deletes
	// (status = 'deleted') and Get filters those out, so both "never connected" and
	// "deliberately disconnected" arrive as domain.ErrNotFound — and only the first of those
	// may fall back to the LF-owned account. It is on the INTERFACE rather than behind a type
	// assertion so a reader that cannot answer fails to compile, instead of silently taking
	// the fallback that the assertion's else-branch would give it.
	Disconnected(ctx context.Context, projectID string, provider model.Provider) (bool, error)
}

// credsSource resolves a project's decrypted platform credentials. It is the ONLY
// shared piece across adapters: the mechanical Get-then-Decrypt. It deliberately does
// NOT interpret the plaintext — credential shapes differ per platform (OAuth2 refresh
// tokens, OAuth1 4-tuples, static bearer tokens), so each adapter unmarshals the blob
// itself. ProviderConfig (non-secret columns) and AccountID come back untouched too.
type credsSource struct {
	repo connReader
	enc  domain.Encryptor
	// cache holds DECRYPTED credentials keyed by (project, provider) and validated against
	// the connection row's version on every read, so a rotated or revoked credential can
	// never be served. See credCache for why the row itself is deliberately NOT cached, and
	// what that buys across replicas.
	cache *credCache
}

func newCredsSource(repo connReader, enc domain.Encryptor) *credsSource {
	return &credsSource{repo: repo, enc: enc, cache: sharedCredCache(repo, enc)}
}

// resolved carries a connection's decrypted credential bytes plus the non-secret
// fields an adapter reads (account id, provider-specific config columns). The
// plaintext is raw JSON the caller unmarshals into its own credential struct.
type resolved struct {
	// connID and version identify the connection ROW these credentials were decrypted from.
	// They ride along so a caller that builds an expensive object from this credential (a
	// platform client, which owns its own OAuth token cache) can cache it under the same
	// identity the credential cache uses, and have it invalidated by the same rotation.
	connID  string
	version int64

	plaintext      []byte
	accountID      string
	label          string // the connection's friendly name (Connection.Label column)
	providerConfig map[string]string
	status         model.ConnectionStatus
	// fromSystem records that these credentials came from the LF system fallback, not
	// from a row the project owns. Defects found LATER — by an adapter's validator, not
	// by resolveConn — are otherwise indistinguishable, and misattributing them sends a
	// project to edit a connection it does not have. See systemScoped.
	fromSystem bool
}

// systemScoped re-attributes an unusable-connection error to the LF system row when that is
// where the credentials came from. A no-op for every other error and every project-owned
// connection, so callers can apply it unconditionally.
// systemOrigin marks err as having come from the system row, for EVERY error the fallback
// produces rather than only the ones systemScoped classifies. Applied at the single site that
// knows the fallback was taken, so no later handler has to reconstruct it.
func systemOrigin(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", domain.ErrSystemConnectionOrigin, err)
}

func (r *resolved) systemScoped(err error) error {
	if r == nil || !r.fromSystem || err == nil || !errors.Is(err, domain.ErrConnectionNotUsable) {
		return err
	}
	// Idempotent, so "callers can apply it unconditionally" holds even where two layers
	// each apply it — a validator that tags its own returns, and a caller that tags
	// everything it gets back. Without this the chain carries the sentinel twice and
	// renders the prefix twice, which is what pushed the tagging up to one caller per
	// path in the first place; that arrangement is exactly what let the toggle, metrics
	// and create paths silently omit it while discovery had it.
	if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		return err
	}
	return fmt.Errorf("%w: %w", domain.ErrSystemConnectionNotUsable, err)
}

// resolve fetches the project's connection for the provider and decrypts its
// credentials, falling back to the reserved system scope (model.SystemProjectID) when
// the project has no connection of its own. It returns a NOT-created error (so the
// orchestrator releases the dispatch claim) when neither scope yields a usable
// connection — none of those states could have created an upstream campaign.
func (s *credsSource) resolve(ctx context.Context, projectID string, provider model.Provider) (*resolved, error) {
	conn, err := s.repo.Get(ctx, projectID, provider)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// No connection of its own: fall back to the LF-owned system account. ONLY a
			// genuine absence falls back — every other failure means the project HAS a
			// connection needing attention, and running its campaign on the LF account
			// would spend LF money on a request it believed was its own.
			if sysConn, sysErr := s.systemConn(ctx, projectID, provider); sysErr != nil {
				return nil, sysErr
			} else if sysConn != nil {
				// Keep the ORIGIN of any defect. Downstream this is the difference
				// between a 400 telling the project to edit a connection it does not
				// have, and a 5xx paging whoever installed the LF credential.
				res, rerr := s.resolveConn(ctx, model.SystemProjectID, sysConn, provider)
				if res != nil {
					res.fromSystem = true
				}
				return res, systemOrigin((&resolved{fromSystem: true}).systemScoped(rerr))
			}
			// Wrap the sentinel rather than dropping it: read-only callers such as
			// account discovery need to tell "this project has no connection" (404)
			// apart from "the platform call failed" (503). The dispatch paths only
			// consult NoUpstreamCreate, so preserving it changes nothing for them.
			// It names the PROJECT even though two lookups missed: which one was absent
			// is an operator's question, and systemConn logs it.
			return nil, noOwnConnection(projectID, provider)
		}
		return nil, connLoadFailed(provider, err)
	}
	return s.resolveConn(ctx, projectID, conn, provider)
}

// resolveOwned is resolve WITHOUT the system fallback: it consults the project's own scope
// and nothing else.
//
// It exists for adoption, and the reason it is a separate resolution rather than a check on
// resolve's result is that the fallback's failures are not adoption's to report. resolve
// LOADS, VALIDATES and DECRYPTS the system row before a caller can inspect `fromSystem`, so
// an LF system connection that is missing its credential blob, or that no longer decrypts,
// surfaces as ErrSystemConnectionNotUsable — a 500 that pages whoever installed the LF
// credential. On this path that is the wrong answer twice over: the caller's own remedy is
// the actionable 409 ("connect your own ad account"), and the row being complained about is
// one adoption would have refused even in perfect health. Inspecting `fromSystem` after the
// fact cannot fix it, because the error is returned INSTEAD of the resolved value.
//
// Declining to look is also the cheaper contract to keep: it makes the refusal independent of
// the system row's state, so no future failure mode of the fallback can leak onto this path
// and need a new sentinel arm here.
func (s *credsSource) resolveOwned(ctx context.Context, projectID string, provider model.Provider) (*resolved, error) {
	conn, err := s.repo.Get(ctx, projectID, provider)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noOwnConnection(projectID, provider)
		}
		return nil, connLoadFailed(provider, err)
	}
	return s.resolveConn(ctx, projectID, conn, provider)
}

// noOwnConnection reports that the project has no connection of its own — after the fallback
// declined to supply one (resolve) or was never offered (resolveOwned).
//
// It wraps the sentinel rather than dropping it: read-only callers such as account discovery
// need to tell "this project has no connection" (404) apart from "the platform call failed"
// (503), and adoption maps it to its own permanent refusal. The dispatch paths only consult
// NoUpstreamCreate, so preserving it changes nothing for them. It names the PROJECT even
// where two lookups missed: which one was absent is an operator's question, and systemConn
// logs it.
func noOwnConnection(projectID string, provider model.Provider) error {
	return notCreated(fmt.Errorf("no %s connection configured for project %s: %w", provider, projectID, domain.ErrNotFound))
}

// connLoadFailed reports a repo failure loading a connection. A DB error is NOT a pre-create
// signal we can prove, but no upstream call was made either — the create never started — so
// it is not-created and a transient blip does not wedge the claim.
func connLoadFailed(provider model.Provider, err error) error {
	return notCreated(fmt.Errorf("load %s connection: %w", provider, err))
}

// systemConn loads the reserved system-scope connection: (nil, nil) when no system account
// is configured — the ordinary state — and an error only when the lookup itself failed.
func (s *credsSource) systemConn(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error) {
	if projectID == model.SystemProjectID {
		// Already the system scope: a second identical Get would return the same miss.
		return nil, nil
	}
	// The fallback is an ad-ACCOUNT fallback, and credsSource is shared by more than the
	// ad paths: AudienceBuilder resolves ProviderHubSpot through this same function. What
	// falls back there is not a budget but a CRM portal, so a project with no HubSpot
	// connection would have its contact lists written into the LF's own portal — real
	// contact data landing in the wrong tenant, silently, and contradicting the documented
	// behaviour that the build fails. Spending LF ad budget on an LF-run campaign is the
	// deliberate trade this fallback makes; mixing tenants' contacts is not the same trade
	// and was never the intent.
	//
	// Asked as a CLASSIFICATION rather than as `provider != ProviderHubSpot`, per Kind()'s
	// own guidance: a provider added later returns "" from Kind() until someone classifies
	// it, so it is denied the LF credential by default instead of inheriting it.
	if !provider.IsPaidAds() {
		return nil, nil
	}
	// A project that DISCONNECTED its account said something, and the LF account is not it.
	// Delete soft-deletes and Get filters status = 'deleted' out, so an explicit disconnect
	// reaches the caller as the same domain.ErrNotFound as never having connected at all —
	// and the branch above reads that as licence to spend LF budget on that project's
	// campaigns. Absence of a statement is what this fallback is for; a statement to the
	// contrary is not absence.
	//
	// Fails CLOSED on a probe error: an unanswered "was this disconnected?" is not a no.
	switch disconnected, derr := s.repo.Disconnected(ctx, projectID, provider); {
	case derr != nil:
		return nil, notCreated(fmt.Errorf("check whether project %s disconnected its %s account: %w",
			projectID, provider, derr))
	case disconnected:
		slog.InfoContext(ctx, "project disconnected its account; not falling back to the system account",
			"project_id", projectID, "provider", string(provider))
		return nil, nil
	}
	conn, err := s.repo.Get(ctx, model.SystemProjectID, provider)
	if err == nil {
		slog.InfoContext(ctx, "project has no connection; using the system account",
			"project_id", projectID, "provider", string(provider))
		return conn, nil
	}
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	// A repo failure on the system lookup is NOT an absence: reporting it as one hands
	// the caller a 404 saying "you have no connection" when the database did not answer.
	return nil, notCreated(fmt.Errorf("load system %s connection: %w", provider, err))
}

// resolveConn validates a connection row and decrypts its credentials. Shared by the
// project scope and the system-account fallback, so a system row is held to exactly the
// same standard rather than trusted because it is ours. Status rides out on `resolved`.
func (s *credsSource) resolveConn(ctx context.Context, projectID string, conn *model.Connection, provider model.Provider) (*resolved, error) {
	// Serve a previously decrypted credential ONLY when it was decrypted from the version
	// this very call just read. conn came from the repository moments ago, so the version
	// below is current: a rotation, a re-point, a credential replacement or a delete has
	// already bumped it (every mutating statement in ConnectionRepo does), and the entry
	// misses. That is the invalidation mechanism — it needs no eviction hook, and because
	// the version lives in Postgres it holds on every replica rather than only the one that
	// handled the write.
	//
	// The cache is consulted AFTER the callers above have established which SCOPE this
	// connection came from, and the key carries that scope's projectID: a project resolving
	// through the LF system fallback caches under model.SystemProjectID, so it can neither
	// poison nor read a project-owned entry.
	key := cacheKeyFor(projectID, provider)
	if cached, ok := s.cache.get(key, conn.ID, conn.Version); ok {
		return cached.clone(), nil
	}

	// The branches below are tagged with domain.ErrConnectionNotUsable; the two above
	// deliberately are not. The distinction is whether the connection ROW itself is the
	// thing that needs editing. A row with no credential blob is permanently unusable as
	// it stands and no amount of retrying changes that — read-only callers must answer
	// 400, not 503. A missing connection (ErrNotFound) is a 404 and a repo failure is a
	// genuine "try again later", so flattening either into "not usable" would destroy a
	// distinction the service layer depends on.
	if len(conn.EncryptedCredentials) == 0 {
		// Both sentinels, not just the status one. ErrConnectionNotUsable answers "what
		// HTTP status", ErrCredentialsAbsent answers "why" — and the discovery handler
		// logs the why from a fixed vocabulary (unusableConnectionReason). Wrapping only
		// the status sentinel logged this as "unclassified", i.e. "cause unknown", for
		// the single most knowable failure in the set.
		return nil, notCreated(fmt.Errorf("%s connection for project %s has no stored credentials: %w: %w",
			provider, projectID, domain.ErrConnectionNotUsable, domain.ErrCredentialsAbsent))
	}
	// Coalesce concurrent misses for the same (project, provider, row id, version): N callers
	// arriving together perform ONE decrypt instead of N. Keyed with the row id AND the version
	// so a burst either side of a rotation — or either side of a disconnect/reconnect, which
	// restarts version at 1 on a NEW row — cannot be served the other's plaintext.
	//
	// ONE DECRYPT is the whole of this guarantee. It deliberately does not claim one token
	// exchange: every caller receives its own clone() below, each builds its own platform
	// client, and the OAuth token is cached on the client INSTANCE — so coalescing the decrypt
	// changes nothing downstream by itself. Collapsing the token exchange takes a separate
	// cache of the built CLIENT, which today exists only for Google Ads (clientCache, whose
	// buildOnce coalesces construction); Reddit and Microsoft still rebuild per resolve and
	// still re-mint. An earlier version of this comment claimed the token saving here, which
	// was the same conflation the PR's original decrypt-count measurement rested on.
	shared, err := s.cache.decryptOnce(key, conn.ID, conn.Version, func() (*resolved, error) {
		return s.decryptConn(ctx, key, projectID, conn, provider)
	})
	if err != nil {
		return nil, err
	}
	// Every caller — the singleflight leader and its followers alike — gets its OWN copy.
	// resolve stamps fromSystem on the value it hands back, and that is a property of how
	// THIS call resolved, not of the credential: handing several callers one pointer would
	// let a fallback resolution's attribution appear on a project-owned one, and would be a
	// write race between goroutines besides.
	return shared.clone(), nil
}

// decryptConn performs the actual decrypt and builds the resolved value, storing it in the
// cache on success. Split out of resolveConn so the singleflight leader runs exactly this
// body and every follower shares its result.
func (s *credsSource) decryptConn(ctx context.Context, key credCacheKey, projectID string, conn *model.Connection, provider model.Provider) (*resolved, error) {
	plaintext, derr := s.enc.Decrypt(conn.EncryptedCredentials)
	if derr != nil {
		// derr is NOT echoed to callers by the service layer — a decrypt failure can
		// carry ciphertext detail — and whether it is LOGGED depends on the HANDLER, not
		// on this function.
		//
		// On the campaign toggle and metrics handlers (`internal/service/brief.go`), as of
		// LFXV2-3065, neither classification logs the cause. The 500 arm
		// (authenticated-decryption failure) used to, on the reasoning that the error is
		// built by the encryptor from ciphertext and key material only: true of the
		// SENTINEL, but what reaches the log is the whole chain, and `domain.Encryptor` is
		// an INTERFACE whose implementations are free to quote the ciphertext or key
		// material they failed on. The ErrConnectionNotUsable arm (409 on these two
		// handlers; 400 on account discovery, which classifies the same sentinel for its
		// own caller) never did: it logs a fixed reason token and nothing else, since the
		// conditions reaching it include one detected by decoding the DECRYPTED blob.
		// Both are pinned by
		// `Test{ToggleCampaignStatus,GetCampaignMetrics}_DecryptFailureLogsNoErrorText`.
		// Do not "restore" logging of the cause on either.
		//
		// Account DISCOVERY still logs the full `aerr` (`internal/service/connection.go`,
		// the ErrCredentialDecryptionFailed arm). That is out of this change's scope and is
		// NOT covered by the tests above — so this is deliberately not a service-wide
		// guarantee. Anything relying on one must close that path first.
		// All arms return a fixed message to the caller regardless.
		//
		// A decrypt failure is NOT one condition, and which sentinel it carries decides
		// whether a human edits a connection or ops gets paged. Only a blob the encryptor
		// could not even attempt to authenticate (domain.ErrCredentialsMalformed — for
		// AES-GCM, shorter than a nonce PLUS the authentication tag) is proven bad row
		// data, and only that earns ErrConnectionNotUsable → 400. A GCM AUTHENTICATION
		// failure keeps its own classification: a wrong or rotated application key, or
		// tampering/corruption of this one row. GCM cannot tell those apart, so the
		// sentinel does not claim to either — it says "authentication failed", and how
		// many projects are affected is what tells a responder which it was.
		// Reported as "not usable as configured" it would tell an operator to fix a
		// connection that may be fine, and would bury the 500 that surfaces a broken
		// deployment key. An unrecognised decrypt error takes that same path on purpose: an
		// encryptor that proves nothing about the row must not be read as proving the row
		// is at fault.
		if errors.Is(derr, domain.ErrCredentialsMalformed) {
			return nil, notCreated(fmt.Errorf("decrypt %s credentials: %w: %w", provider, domain.ErrConnectionNotUsable, derr))
		}
		return nil, notCreated(fmt.Errorf("decrypt %s credentials: %w: %w", provider, domain.ErrCredentialDecryptionFailed, derr))
	}
	res := &resolved{
		connID:         conn.ID,
		version:        conn.Version,
		plaintext:      plaintext,
		accountID:      conn.AccountID,
		label:          conn.Label, // the friendly name lives on the shared column, not ProviderConfig
		providerConfig: conn.ProviderConfig,
		status:         conn.Status,
	}
	// Stored WITHOUT fromSystem: that flag is set by the caller that took the fallback, and
	// it is a property of how this call resolved rather than of the credential. Baking it
	// into the shared entry would let one path's attribution leak into another's.
	s.cache.put(key, conn.ID, conn.Version, res)
	return res, nil
}

// clone returns a shallow copy safe for one caller to stamp its own attribution on.
//
// Shallow is correct and deliberate: the fields it shares — the plaintext bytes, the
// providerConfig map — are treated as READ-ONLY by every consumer (adapters unmarshal the
// plaintext and read config values; none writes back), so copying them per call would burn
// an allocation per resolve to defend against a mutation nobody performs. What must not be
// shared is the struct itself, because fromSystem IS written per call.
func (r *resolved) clone() *resolved {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// preCreateError marks a dispatch failure that happened BEFORE any upstream (paid)
// create call — missing/invalid connection, config/validation errors, credential
// unmarshal failures. The orchestrator detects NoUpstreamCreate() (via errors.As) and
// RELEASES the dispatch claim so a retry is safe. Anything NOT wrapped this way is
// treated conservatively (claim retained) because the create may have landed.
type preCreateError struct{ err error }

func (e *preCreateError) Error() string          { return e.err.Error() }
func (e *preCreateError) Unwrap() error          { return e.err }
func (e *preCreateError) NoUpstreamCreate() bool { return true }

// notCreated wraps err as a preCreateError (the request definitely did not create
// anything upstream).
func notCreated(err error) error { return &preCreateError{err: err} }

// campaignStatusCreated is the status stamped on a campaign row after a successful
// upstream create. The orchestrator does NOT set a status on success (it only sets
// "pending" for a retained ambiguous orphan), and CampaignRepo.UpsertCampaign writes
// Status verbatim — so the dispatcher must supply a non-empty status or the row
// persists with an empty one.
const campaignStatusCreated = "created"

// campaignStatusCreatedDegraded marks a campaign that WAS created upstream but whose
// outcome is incomplete — a per-platform partial (e.g. a promoted-post/ad step that
// failed or is unconfirmed, or fewer ads created than requested). It is a distinct,
// VISIBLE status (vs the clean campaignStatusCreated) so a degraded outcome is not
// silently "succeeded": the campaign exists (returning an error would mislead and be
// unrecoverable by retry, since idempotency short-circuits a re-dispatch), so instead
// the degraded state is persisted for a human/monitor to reconcile. Shared by every
// dispatch adapter (reddit/meta/twitter) whose client can return a partial success.
const campaignStatusCreatedDegraded = "created_degraded"

// unmarshalPlatformConfig extracts ONE platform's nested config object from the
// per-request config envelope and unmarshals it into dst. The CreateCampaigns
// request carries a single `config` blob for all selected platforms, with each
// platform's params nested under its own key (redditConfig / linkedInConfig /
// metaConfig / twitterConfig — see docs/api-catalog.md). Unmarshalling the whole
// envelope directly into a platform struct would silently read nothing (or the wrong
// keys). An absent key is not an error — it yields a zero-value config. A present but
// malformed value is an error.
func unmarshalPlatformConfig(envelope []byte, key string, dst any) error {
	if len(envelope) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &m); err != nil {
		return fmt.Errorf("decode campaign config envelope: %w", err)
	}
	raw, ok := m[key]
	if !ok || len(raw) == 0 {
		return nil // no per-platform config supplied; zero value is fine
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	return nil
}

// envelopeHSToken extracts the OPTIONAL top-level `hsToken` from the campaign config
// envelope. Per docs/api-catalog.md `hsToken` is a TOP-LEVEL field (sibling to the
// per-platform config objects like redditConfig/metaConfig), NOT nested inside them —
// so it is read from the envelope here, shared by every dispatcher. Returns ("", nil)
// when the envelope is empty or the field is absent. Returns an ERROR when the envelope
// is malformed JSON, or when `hsToken` is present but not a string — including an
// explicit `null` (a wrong-typed documented field is a caller error, not a silent
// fallback).
func envelopeHSToken(envelope []byte) (string, error) {
	if len(envelope) == 0 {
		return "", nil
	}
	// Decode into a map of raw messages to PRESERVE field presence. A struct field of
	// type *json.RawMessage would be set to nil for BOTH an absent field AND an explicit
	// `null` (Go's decoder nils the pointer on JSON null), making the two
	// indistinguishable — so an explicit `null` would slip through the absent path. With
	// a map, the KEY is present iff the field appears, and its value carries the literal
	// bytes ("null" for JSON null).
	var m map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &m); err != nil {
		// The envelope as a whole is malformed. The caller already validated it via
		// unmarshalPlatformConfig, so this is defensive; surface it rather than swallow.
		return "", fmt.Errorf("decode campaign config envelope: %w", err)
	}
	raw, present := m["hsToken"]
	if !present {
		return "", nil // field absent — fine, caller falls back to the brief token
	}
	// The field is PRESENT. An explicit `null` is a present-but-not-a-string value, so
	// it is a caller error (not the silent absent/fallback path) — consistent with the
	// number/object cases below. json.Unmarshal("null", &s) is a no-op that would leave
	// s="" WITHOUT an error, so `null` must be rejected explicitly.
	if strings.TrimSpace(string(raw)) == "null" {
		return "", fmt.Errorf("config hsToken must be a string, got null")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// hsToken is PRESENT but not a string (e.g. a number/object). Do NOT silently
		// swallow it and fall back — a wrong-typed documented field is a caller error.
		return "", fmt.Errorf("config hsToken must be a string: %w", err)
	}
	return strings.TrimSpace(s), nil
}

// cacheIdentity returns the (key, row id, version) triple that identifies the connection these
// credentials came from. A caller caching an object BUILT from the credential — a platform client
// holding its own OAuth token — uses this so its entry is invalidated by exactly the same rotation
// that invalidates the credential.
//
// The key names the SCOPE the credential came from, not the project that asked for it. Under the
// LF system fallback every project with no connection of its own resolves the SAME system row, and
// credsSource.resolveConn already caches that plaintext under model.SystemProjectID for exactly
// that reason. Keying a client by the CALLING project instead would give each fallback project its
// own client for one shared row — and because the OAuth token is cached on the client instance,
// each would mint its own access token: measured at one token exchange per project, which is the
// per-call exchange this cache exists to remove. Scoping the key here makes the client cache reuse
// as wide as the credential cache's, and no wider — a project-owned row still keys on its own
// project, so no project can be served another's client.
func (r *resolved) cacheIdentity(projectID string, provider model.Provider) (credCacheKey, string, int64) {
	scope := projectID
	if r.fromSystem {
		scope = model.SystemProjectID
	}
	return cacheKeyFor(scope, provider), r.connID, r.version
}

// stampProvenance records, on the campaign a dispatcher is about to return, WHICH AD
// ACCOUNT served it: the project's own connection, or the LF-owned system account the
// fallback reaches when the project has none.
//
// `fromSystem` is known here and nowhere else. It is stamped on the resolved credential
// at the single site that takes the fallback (resolve, above), and before this existed it
// was consumed for error attribution and then DISCARDED — so the persisted campaign row
// recorded the project but never the credential that served it, and no query could answer
// which campaigns the LF paid for. This is the one place with both facts in scope.
//
// Applied by each Dispatch through a DEFER on its named return rather than at each
// `return campaignFromX(...)` site. That is deliberate: the seven dispatchers have two or
// three success/partial returns each, several of which return a campaign ALONGSIDE an
// error (the UNCONFIRMED and degraded paths), and those are exactly the rows an operator
// reconciling system-account spend cannot afford to have unstamped. Stamping per return
// site would mean seventeen edits that a future eighth path silently omits, and the
// omission would look identical to a campaign that genuinely ran on a project's own
// account — a false FALSE, which the migration explains is the failure this column must
// not have. A deferred call on the named return covers every exit, including ones not
// written yet.
//
// Nil-safe on both sides. A dispatcher that returns (nil, err) has no row to stamp, and a
// credential that was never resolved (r == nil) knows nothing to stamp with — in both
// cases the campaign keeps whatever it had, which for a nil resolved is "unknown" (NULL),
// never a fabricated false.
func (r *resolved) stampProvenance(c *model.Campaign) {
	if c == nil || r == nil {
		return
	}
	fromSystem := r.fromSystem
	c.RanOnSystemAccount = &fromSystem
}

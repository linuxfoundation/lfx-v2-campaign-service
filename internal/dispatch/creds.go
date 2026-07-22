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
}

// credsSource resolves a project's decrypted platform credentials. It is the ONLY
// shared piece across adapters: the mechanical Get-then-Decrypt. It deliberately does
// NOT interpret the plaintext — credential shapes differ per platform (OAuth2 refresh
// tokens, OAuth1 4-tuples, static bearer tokens), so each adapter unmarshals the blob
// itself. ProviderConfig (non-secret columns) and AccountID come back untouched too.
type credsSource struct {
	repo connReader
	enc  domain.Encryptor
}

func newCredsSource(repo connReader, enc domain.Encryptor) *credsSource {
	return &credsSource{repo: repo, enc: enc}
}

// resolved carries a connection's decrypted credential bytes plus the non-secret
// fields an adapter reads (account id, provider-specific config columns). The
// plaintext is raw JSON the caller unmarshals into its own credential struct.
type resolved struct {
	plaintext      []byte
	accountID      string
	label          string // the connection's friendly name (Connection.Label column)
	providerConfig map[string]string
	status         model.ConnectionStatus
}

// resolve fetches the project's connection for the provider and decrypts its
// credentials. It returns a NOT-created error (so the orchestrator releases the
// dispatch claim) when the connection is missing, unconfigured, or undecryptable —
// none of those could have created an upstream campaign.
func (s *credsSource) resolve(ctx context.Context, projectID string, provider model.Provider) (*resolved, error) {
	conn, err := s.repo.Get(ctx, projectID, provider)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, notCreated(fmt.Errorf("no %s connection configured for project %s", provider, projectID))
		}
		// A repo error (DB down) is NOT a pre-create signal we can prove, but no
		// upstream call was made either — the create never started. Treat as
		// not-created so a transient DB blip doesn't wedge the claim.
		return nil, notCreated(fmt.Errorf("load %s connection: %w", provider, err))
	}
	if len(conn.EncryptedCredentials) == 0 {
		return nil, notCreated(fmt.Errorf("%s connection for project %s has no stored credentials", provider, projectID))
	}
	plaintext, derr := s.enc.Decrypt(conn.EncryptedCredentials)
	if derr != nil {
		return nil, notCreated(fmt.Errorf("decrypt %s credentials: %w", provider, derr))
	}
	return &resolved{
		plaintext:      plaintext,
		accountID:      conn.AccountID,
		label:          conn.Label, // the friendly name lives on the shared column, not ProviderConfig
		providerConfig: conn.ProviderConfig,
		status:         conn.Status,
	}, nil
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
// so it is read from the envelope here, shared by every dispatcher. Returns "" when
// absent or when the envelope is empty/unparseable (a malformed envelope surfaces via
// unmarshalPlatformConfig's own error on the same input).
func envelopeHSToken(envelope []byte) string {
	if len(envelope) == 0 {
		return ""
	}
	var m struct {
		HSToken string `json:"hsToken"`
	}
	if err := json.Unmarshal(envelope, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.HSToken)
}

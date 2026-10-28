// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// errReadOnly is what every write this command could be asked to make answers.
var errReadOnly = errors.New("verify-metrics is read-only")

// connLookup is the only part of the connection repository the dispatchers are given: the two
// reads credential resolution makes. Handing them the repository itself would expose its write
// methods to any type assertion a dispatcher makes.
type connLookup interface {
	Get(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error)
	Disconnected(ctx context.Context, projectID string, provider model.Provider) (bool, error)
}

// backend is the database side of a run.
type backend struct {
	conns connLookup
	// campaigns is the campaign repository's READ half: the metrics read looks its row up, and
	// the other reads' orchestrator resolves the project's campaign scope through it.
	campaigns domain.CampaignReader
	enc       domain.Encryptor
	// statsLease is the service's cross-process X stats-job lease (domain.StatsJobLease), so a
	// run that creates X stats jobs (the X audience read and account monitor) takes the same
	// per-account ownership the service's pods take, and never runs jobs alongside one of them
	// or another run. nil only in tests that build a backend by hand.
	statsLease domain.StatsJobLease
	close      func()
}

// openLiveBackend loads the database and encryption key the way the service's
// bootstrap-system-account subcommand does — config.ResolveDatabaseURL (DATABASE_URL or the
// chart's PG* variables) and CREDENTIAL_ENCRYPTION_KEY — but opens the session READ ONLY.
func openLiveBackend(ctx context.Context) (*backend, error) {
	dsn, err := config.ResolveDatabaseURL()
	if err != nil {
		return nil, fmt.Errorf("resolve database settings: %w", err)
	}
	if dsn == "" {
		return nil, fmt.Errorf("no database configured; set PGHOST/PGUSER/PGPASSWORD/PGDATABASE or %s", constants.EnvDatabaseURL)
	}
	enc, err := crypto.NewAESGCMFromBase64(os.Getenv(constants.EnvCredentialEncryptionKey))
	if err != nil {
		return nil, fmt.Errorf("init credential encryptor (%s): %w", constants.EnvCredentialEncryptionKey, err)
	}
	pool, err := openReadOnlyPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	// The lease is a session-level advisory lock (pg_try_advisory_lock), which a READ ONLY
	// session may take: it writes no row. Its session is opened outside the pool from the
	// pool's ConnConfig, which carries default_transaction_read_only (readOnlyPoolConfig).
	lease := postgres.NewStatsJobLease(pool)
	return &backend{
		conns:      postgres.NewConnectionRepo(pool),
		campaigns:  postgres.NewCampaignRepo(pool),
		enc:        enc,
		statsLease: lease,
		close: func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), statsLeaseCloseTimeout)
			lease.Close(closeCtx)
			cancel()
			pool.Close()
		},
	}, nil
}

// statsLeaseCloseTimeout bounds releasing the X stats-job lease at exit.
const statsLeaseCloseTimeout = 5 * time.Second

// dsnSourceVerifyMetrics names where this command's DSN came from, for an operator who has to
// go and fix it. config.ResolveDatabaseURL reads DATABASE_URL first and otherwise composes one
// from the chart's PG* variables, so the answer is one of two places and the message says both
// rather than guessing which.
const dsnSourceVerifyMetrics = "the database settings (" + constants.EnvDatabaseURL + " or the PG* variables)"

// openReadOnlyPool opens a small pool whose every session is READ ONLY, so no statement this
// process issues can write, whichever repository method runs it.
//
// It is a hand-rolled sibling of postgres.NewPool — same three arms, same order — and it carries
// the same obligation: no arm may render a DSN identifier into an error. ParseConfig's message
// is replaced outright, because pgx's ParseConfigError prints the whole connection string
// through redactPW, which masks the password and keeps the username. The connect and ping arms
// go through postgres.SafeDSNErr, which compares the driver's text against the identifiers
// actually configured and withholds the message wholesale when it names one — pgconn's
// ConnectError embeds a `user=%s database=%s` fragment on every connection failure, and the
// server's own 28P01/28000/3D000 diagnostics quote the role and database verbatim.
//
// The redacted text is interpolated with %s rather than wrapped with %w deliberately: wrapping
// would leave the driver's original message reachable through errors.As for any later caller to
// print. Nothing in this command inspects this error's chain — run only matches flag.ErrHelp —
// so there is no behaviour to preserve, and dropping the cause is the safe direction.
// readOnlyPoolConfig is the pool configuration: every session READ ONLY twice over. The
// default_transaction_read_only startup parameter rides in ConnConfig, so it also covers a
// session opened OUTSIDE the pool from a copy of that config — the X stats-job lease opens its
// session that way (postgres.StatsJobLease), and AfterConnect never runs for it. AfterConnect
// stays as the pooled sessions' second guard.
func readOnlyPoolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse database config: invalid database settings (redacted; check host/port/params)")
	}
	cfg.MaxConns = 2
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY")
		return err
	}
	return cfg, nil
}

func openReadOnlyPool(ctx context.Context, dsn string) (*postgres.Pool, error) {
	cfg, err := readOnlyPoolConfig(dsn)
	if err != nil {
		return nil, err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %s", postgres.SafeDSNErr(dsn, dsnSourceVerifyMetrics, err))
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("ping database: %s", postgres.SafeDSNErr(dsn, dsnSourceVerifyMetrics, err))
	}
	return &postgres.Pool{Pool: p}, nil
}

// readOnlyConns narrows the connection repository to connLookup.
type readOnlyConns struct{ r connLookup }

func (c *readOnlyConns) Get(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error) {
	return c.r.Get(ctx, projectID, provider)
}

func (c *readOnlyConns) Disconnected(ctx context.Context, projectID string, provider model.Provider) (bool, error) {
	return c.r.Disconnected(ctx, projectID, provider)
}

// readOnlyCampaigns is the domain.CampaignRepository the orchestrator is built on: the reader's
// methods, and every write refused. NewOrchestrator takes the whole repository; none of the reads
// this command runs writes a campaign, and if one ever did it would fail here, before the READ
// ONLY session refused it too.
type readOnlyCampaigns struct{ domain.CampaignReader }

var _ domain.CampaignRepository = (*readOnlyCampaigns)(nil)

func (*readOnlyCampaigns) UpsertCampaign(context.Context, *model.Campaign, domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return nil, errReadOnly
}

func (*readOnlyCampaigns) AdoptCampaign(context.Context, *model.Campaign, int64, domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return nil, errReadOnly
}

func (*readOnlyCampaigns) ReplaceCampaign(context.Context, *model.Campaign, int64, domain.CampaignLockToken, domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return nil, errReadOnly
}

func (*readOnlyCampaigns) VerifyClaimedVersion(context.Context, string, string, string, int64, domain.CampaignLockToken) (*model.Campaign, error) {
	return nil, errReadOnly
}

func (*readOnlyCampaigns) ClaimCampaignVersion(context.Context, string, string, string, int64) (*model.Campaign, domain.CampaignLockToken, error) {
	var none domain.CampaignLockToken
	return nil, none, errReadOnly
}

func (*readOnlyCampaigns) ReleaseCampaignLock(context.Context, domain.CampaignLockToken) error {
	return errReadOnly
}

// ReleaseCampaignLockAfterCooldown has no error to return; a read never holds a lock to release.
func (*readOnlyCampaigns) ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken, time.Duration) {}

func (*readOnlyCampaigns) DeleteCampaign(context.Context, string, string, string, int64, *model.Actor, domain.CampaignIndexPayloadFunc) error {
	return errReadOnly
}

// recordingDecryptor decrypts exactly as the wrapped encryptor does and records every value of
// the plaintext in the secret set, so whatever credential the dispatcher actually used is
// scrubbed from all output. Encrypt is refused: nothing in a read needs it.
type recordingDecryptor struct {
	inner   domain.Encryptor
	secrets *secretSet
}

func (d *recordingDecryptor) Encrypt([]byte) ([]byte, error) { return nil, errReadOnly }

func (d *recordingDecryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	plaintext, err := d.inner.Decrypt(ciphertext)
	if err == nil {
		d.secrets.addCredentialDocument(plaintext)
	}
	return plaintext, err
}

// metricsReader is service.MetricsReader: the method the published metrics endpoint calls on a
// dispatcher, restated here so this command depends on the dispatchers rather than the service.
type metricsReader interface {
	ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error)
}

// endpoints overrides the platform hosts. Empty in production; tests point them at an
// httptest server. Deliberately NOT a flag: a flag would let a typo send live credentials to an
// arbitrary host.
type endpoints struct {
	api       string
	token     string
	reporting string
	customer  string // Microsoft Customer Management (the account monitor's currency read)
}

// newDispatcher builds the provider's dispatcher with the service's own constructor, wired to
// the read-only connection reader, the recording decryptor and the guarded HTTP client.
func newDispatcher(p model.Provider, be *backend, secrets *secretSet, hc *http.Client, ep endpoints) (service.PlatformDispatcher, error) {
	conns := &readOnlyConns{r: be.conns}
	enc := &recordingDecryptor{inner: be.enc, secrets: secrets}
	switch p {
	case model.ProviderRedditAds:
		opts := []reddit.Option{reddit.WithHTTPClient(hc)}
		if ep.api != "" {
			opts = append(opts, reddit.WithBaseURL(ep.api))
		}
		if ep.token != "" {
			opts = append(opts, reddit.WithTokenURL(ep.token))
		}
		return dispatch.NewRedditDispatcher(conns, enc, opts...), nil
	case model.ProviderMicrosoftAds:
		opts := []microsoft.Option{microsoft.WithHTTPClient(hc)}
		if ep.api != "" {
			opts = append(opts, microsoft.WithBaseURL(ep.api))
		}
		if ep.token != "" {
			opts = append(opts, microsoft.WithTokenURL(ep.token))
		}
		if ep.reporting != "" {
			opts = append(opts, microsoft.WithReportingBaseURL(ep.reporting))
		}
		if ep.customer != "" {
			opts = append(opts, microsoft.WithCustomerBaseURL(ep.customer))
		}
		return dispatch.NewMicrosoftDispatcher(conns, enc, opts...), nil
	case model.ProviderTwitterAds:
		opts := []twitter.Option{twitter.WithHTTPClient(hc)}
		if ep.api != "" {
			opts = append(opts, twitter.WithBaseURL(ep.api))
		}
		d := dispatch.NewTwitterDispatcher(conns, enc, opts...)
		if be.statsLease != nil {
			d.SetStatsJobLease(be.statsLease)
		}
		return d, nil
	case model.ProviderLinkedInAds:
		opts := []linkedin.Option{linkedin.WithHTTPClient(hc)}
		if ep.api != "" {
			opts = append(opts, linkedin.WithBaseURL(ep.api))
		}
		return dispatch.NewLinkedInDispatcher(conns, enc, opts...), nil
	case model.ProviderMetaAds:
		opts := []meta.Option{meta.WithHTTPClient(hc)}
		if ep.api != "" {
			opts = append(opts, meta.WithBaseURL(ep.api))
		}
		return dispatch.NewMetaDispatcher(conns, enc, opts...), nil
	}
	return nil, fmt.Errorf("provider %q is not supported", p)
}

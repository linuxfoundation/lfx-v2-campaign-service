// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package container provides dependency injection for the application.
package container

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/snowflake"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// sweeperStopTimeout bounds how long Close waits for the stuck-claim sweeper to exit after
// cancellation. Deliberately SMALL: the sweeper is a diagnostic, and this wait precedes the
// dispatch drain, so any time spent here is taken from the budget that protects in-flight
// campaign creation. Exceeding it abandons the goroutine rather than delaying shutdown.
const sweeperStopTimeout = 250 * time.Millisecond

// startupDBTimeout bounds ONE database migration+pool-open attempt. It is a var
// (not a const) only so tests can shrink it; production never changes it.
var startupDBTimeout = 15 * time.Second

// dbRetryInterval is the pause between background DB-init attempts during a cold
// start. A var for the same test-only reason.
var dbRetryInterval = 3 * time.Second

// setBackends is the interface the container needs to late-bind the database once
// the pool opens. Both service types implement it.
type readinessSetter interface {
	SetReadinessDep(service.ReadinessChecker)
}
type backendSetter interface {
	SetBackend(domain.ConnectionRepository, domain.Encryptor)
	// SetOrchestrator injects the orchestrator for account-listing operations.
	// Called by the container after the orchestrator is constructed.
	SetOrchestrator(*service.Orchestrator)
}

// briefBackendSetter is the interface the container needs to late-bind the brief
// service's repos + orchestrator after a cold-start retry opens the pool. *BriefService
// implements it. Kept separate from backendSetter (whose SetBackend has a different
// signature) so the retry path can wire both.
type briefBackendSetter interface {
	SetBackend(domain.BriefRepository, domain.CampaignRepository, domain.JobRepository, *service.Orchestrator)
}

// audienceBackendSetter late-binds the audience repo after a cold-start retry.
// *AudienceService implements it.
type audienceBackendSetter interface {
	SetBackend(domain.AudienceRepository)
	// SetBriefRepo and SetBuilder are part of this interface so the cold-start path CANNOT
	// late-bind the repo while silently leaving the build dependencies nil. It did exactly
	// that: BuildAudience needs all three, so a cold-started pod served every build request
	// with a 503 forever while looking fully wired.
	SetBriefRepo(domain.BriefRepository)
	SetBuilder(service.AudienceBuilder)
}

// notReady is a ReadinessChecker that always reports not-ready. It is wired as
// the health dependency during a cold start so /readyz returns 503 (not OK)
// until the real pool is swapped in — distinct from the no-database mode, where
// no dependency is wired and /readyz reports ready.
type notReady struct{}

func (notReady) Ready(context.Context) bool { return false }

// dispatchDrainTimeout bounds how long Container.Close waits for in-flight
// campaign dispatch to finish before the pool is closed. Together with the
// orchestrator's post-cancel grace (service.CancelGracePeriod) and the sweeper-stop
// wait (sweeperStopTimeout) it forms ContainerCloseTimeout, which is reserved out of
// the overall graceful-shutdown budget (constants.DefaultShutdownTimeout) so the
// HTTP-drain phase and this phase can't sum past it and get SIGKILLed. Validated by
// the init() below.
//
// Sized so sweeperStopTimeout + relayStopTimeout + dispatchDrainTimeout +
// CancelGracePeriod + indexer.DrainTimeout + cooldownStopTimeout leaves a positive
// HTTP-drain budget:
// CancelGracePeriod grew to cover the post-provider persist AND the terminal
// finalize write (both detached, both must complete during grace), so the drain
// window is trimmed to keep the total within DefaultShutdownTimeout. Trimmed again
// (6s -> 4s) when indexer.DrainTimeout joined ContainerCloseTimeout, so the HTTP
// phase keeps a positive budget rather than the total overrunning.
//
// relayStopTimeout bounds the wait for the index relay's in-flight pass at shutdown. Small
// deliberately: this precedes the dispatch drain, and abandoning a pass is safe because
// unpublished rows stay pending for the next process — that is what the outbox is for.
const relayStopTimeout = 250 * time.Millisecond

const dispatchDrainTimeout = 4 * time.Second

// cooldownStopTimeout bounds how long Close waits for in-flight UNCONFIRMED lock cooldowns
// (postgres.ReleaseCampaignLockAfterCooldown) to release their held connection after being
// signalled to cut short. Deliberately SMALL, same as sweeperStopTimeout: the signal makes
// every pending release happen essentially immediately, so this only covers the time for that
// release's own bounded DB round-trip, not the cooldown itself.
//
// It really does bound the connection, not just the wait. StopCooldownsForShutdown passes this
// value down as the unlock round-trip's own deadline, so a stalled unlock fails inside the
// budget and its connection is DESTROYED rather than left checked out — which is what
// pgxpool.Close actually blocks on. Without that hand-off, Close would return after this wait
// while the release ran on for its ordinary 5s bound, and pool.Close below would sit through
// the difference OUTSIDE ContainerCloseTimeout.
const cooldownStopTimeout = 250 * time.Millisecond

// ContainerCloseTimeout is the wall-clock budget for Container.Close: the sweeper-stop
// wait (sweeperStopTimeout), the index relay's stop wait (relayStopTimeout), the
// orchestrator drain (dispatchDrainTimeout), its post-cancel grace
// (service.CancelGracePeriod), the index publisher's connection drain
// (indexer.DrainTimeout), AND the UNCONFIRMED lock cooldown stop wait
// (cooldownStopTimeout). None of these terms are optional bookkeeping — Close really
// does wait on all of them in sequence, so omitting any one understates the phase and
// lets the two phases sum PAST DefaultShutdownTimeout, which is exactly the
// SIGKILL-mid-drain this budget exists to prevent. The server budgets the HTTP-shutdown
// phase and this container-close phase separately (see HTTPShutdownTimeout), so the
// total graceful shutdown is a true sum bounded by constants.DefaultShutdownTimeout.
const ContainerCloseTimeout = sweeperStopTimeout + relayStopTimeout + dispatchDrainTimeout + service.CancelGracePeriod + indexer.DrainTimeout + cooldownStopTimeout

// HTTPShutdownTimeout is the wall-clock budget for draining in-flight HTTP
// handlers before the container is closed. It is whatever remains of the overall
// graceful-shutdown budget after the container-close phase is reserved, so the
// two sequential phases can never sum past DefaultShutdownTimeout (which would
// otherwise risk a SIGKILL mid-drain — the orchestrator's grace timer is
// wall-clock, not bound by a shared context).
const HTTPShutdownTimeout = constants.DefaultShutdownTimeout - ContainerCloseTimeout

// HandlerDrainTimeout is a SEPARATE, dedicated budget for waiting on in-flight
// handler goroutines to return AFTER a forced srv.Close(). It must not be derived
// from the HTTP-shutdown context's remaining time: when srv.Shutdown times out,
// that context is already exhausted, so a "remaining budget" would be zero and
// the wait would return immediately — defeating the tracker exactly when a
// straggler is running. Reserving a small fixed slice guarantees a bounded wait
// in that case. It is carved from HTTPShutdownTimeout so the total still fits.
const HandlerDrainTimeout = 2 * time.Second

func init() {
	// Every term Close actually spends must be inside the budget — including the index
	// publisher's connection drain, which Close performs after the pool closes.
	if ContainerCloseTimeout > constants.DefaultShutdownTimeout {
		panic("ContainerCloseTimeout (sweeper stop + relay stop + dispatch drain + cancel grace + index drain + cooldown stop) exceeds DefaultShutdownTimeout")
	}
	// The HTTP phase must have a positive budget once the container-close phase
	// is reserved; otherwise HTTP handlers would get no drain window at all.
	if HTTPShutdownTimeout <= 0 {
		panic("HTTPShutdownTimeout is non-positive: ContainerCloseTimeout consumes the entire DefaultShutdownTimeout")
	}
}

// Container holds all application dependencies.
type Container struct {
	Config      *config.Config
	Service     svc.Service
	Connections connsvc.Service
	Briefs      briefsvc.Service
	Audiences   audiencesvc.Service

	// mu guards pool, which the background DB-init goroutine sets once the pool
	// opens and Close reads on shutdown.
	mu   sync.Mutex
	pool *postgres.Pool
	orch *service.Orchestrator

	// audienceBuilder performs the platform side of an audience build (Snowflake lookups +
	// HubSpot list creation). Nil when neither is configured, in which case the build endpoint
	// reports a typed 503 and the audience CRUD routes are unaffected.
	audienceBuilder service.AudienceBuilder

	// tokenVerifier verifies the bearer token on every authenticated request. Built ONCE
	// before any wiring branch and injected on every path — same rationale as
	// indexPublisher, with a sharper failure mode: a service constructed without it
	// REJECTS every request, so a missed injection is an outage rather than a quiet
	// return to trusting unverified claims.
	tokenVerifier service.TokenVerifier

	// indexPublisher is built ONCE in NewContainer, before the fast-path/503-mode
	// branch, and injected into every BriefService constructed on either path.
	// Holding it here (rather than building it per-path) is what guarantees the
	// two paths share one NATS connection and that Close can shut it down.
	indexPublisher indexer.Publisher

	// indexRelay drains the index outbox, delivering messages whose direct publish was lost
	// (a process that died between the commit and the publish). Nil when there is no database.
	indexRelay *indexer.Relay

	// cancelSweep stops the periodic stuck-claim sweeper, and sweepDone is closed when
	// it exits so Close can wait for it (both nil when no sweeper runs — i.e. when there
	// is no database). Kept SEPARATE from cancelInit: the sweeper starts once a live pool
	// exists, which happens on either the fast path or the cold-start retry, so it does
	// not share the init goroutine's lifetime.
	cancelSweep context.CancelFunc
	sweepDone   chan struct{}

	// cancelInit stops the background DB-init goroutine (nil when none runs).
	cancelInit context.CancelFunc
	// initDone is closed when the background goroutine exits, so Close can wait
	// for it (nil when no goroutine runs).
	initDone chan struct{}

	// snowflakeClient is the optional Snowflake connection pool for audience enrichment.
	// Stored here so it can be closed during Container.Close(), releasing database sessions
	// and connection pool resources. Nil when Snowflake is not configured.
	snowflakeClient *snowflake.Client
}

// NewContainer creates and wires all application dependencies.
//
// If a database URL is configured it runs migrations and opens the pool. On
// SUCCESS everything is wired against the live pool immediately. On a TRANSIENT
// failure (database unreachable / migration deadline) the container does NOT
// fail the process: it boots the services in 503 mode (health reports not-ready,
// connection endpoints return the typed 503) and retries migration+pool in the
// BACKGROUND, swapping the live pool in once it opens. This is what makes the
// deployment's ~90s startupProbe budget real: /readyz stays 503 during a DB cold
// start and the pod is kept alive, rather than the process exiting at the first
// 15s attempt and crash-looping.
//
// Config errors that a retry cannot fix (invalid database settings, a bad
// credential-encryption key) still fail fast — those return an error and the
// process exits.
//
// If no database URL is configured, the connection service is wired with a nil
// repo so its routes stay mounted and return the typed 503 ServiceUnavailable
// from the OpenAPI contract instead of a bare 404; the health endpoints report
// ready in that mode.
func NewContainer(cfg *config.Config) (container *Container, err error) {
	slog.Info("initializing dependency container")

	if err := cfg.ValidateDatabaseSettings(); err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}

	c := &Container{Config: cfg}

	// Build the index publisher BEFORE any wiring branch. Indexing is independent of
	// the database (a resource is published after its write commits), and there are
	// THREE paths that construct a BriefService — no-database mode, the live fast
	// path, and 503-mode + its background retry. Building it here and injecting the
	// same instance everywhere is what stops a path from silently keeping the Noop
	// and publishing nothing. It is a Noop when NATS is unconfigured or unreachable.
	indexPub, iperr := newIndexPublisher(cfg)
	if iperr != nil {
		return nil, iperr
	}
	c.indexPublisher = indexPub
	// newIndexPublisher can return a LIVE NATS connection with a reconnect goroutine
	// behind it, and every failure below returns a NIL container — which leaves the
	// caller no handle to Close, and nothing else stops that goroutine. A long-lived
	// caller (a test binary, most visibly) then leaks a connection and its background
	// work per failed construction. Closing here on the error path, rather than at each
	// return, is what keeps the next failure added below from reintroducing this.
	defer func() {
		if err != nil {
			indexPub.Close()
		}
	}()

	// Build the token verifier before any wiring branch, for the same reason and with
	// one extra: an unusable JWKS configuration must stop the pod here. A verifier that
	// cannot verify has only two possible behaviours and both are wrong — refusing
	// everything is an outage with a confusing cause, allowing everything is the hole
	// this closes — so the error surfaces as a failed start with the reason attached.
	verifier, verr := auth.New(auth.Config{
		JWKSURL:            cfg.JWKSUrl,
		Audience:           cfg.Audience,
		Issuer:             cfg.Issuer,
		MockLocalPrincipal: cfg.MockLocalPrincipal,
		InCluster:          cfg.InCluster,
	})
	if verr != nil {
		return nil, fmt.Errorf("JWT verification configuration: %w", verr)
	}
	if cfg.MockLocalPrincipal != "" {
		slog.Warn("JWT verification is DISABLED; every request is attributed to the mock principal",
			"principal", cfg.MockLocalPrincipal, "env", constants.EnvMockLocalPrincipal)
	}
	c.tokenVerifier = verifier

	if cfg.DatabaseURL == "" {
		slog.Warn("database URL not set; connection and brief/campaign endpoints will return 503 Service Unavailable")
		c.Service = service.NewCampaignService(nil)
		// Wire the connection + brief services with nil repos so their routes are
		// still mounted and return the typed 503 ServiceUnavailable advertised by
		// the OpenAPI contract, rather than a bare 404 from unmounted routes.
		c.Connections = c.newConnectionService(nil, nil)
		c.Briefs = c.newBriefService(nil, nil, nil, nil)
		c.Audiences = c.newAudienceService(nil, nil)
		slog.Info("dependency container initialized (no database)")
		return c, nil
	}

	// A bad credential-encryption key is a config error, not a transient DB
	// problem, so fail fast (a retry can't fix it).
	enc, err := crypto.NewAESGCMFromBase64(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("init credential encryptor: %w", err)
	}

	// A malformed DATABASE_URL (e.g. a keyword DSN migrations can't consume) is a
	// deterministic config error that NO retry can fix — fail fast rather than
	// entering the background retry loop and 503-looping forever. (Transient
	// DB-unavailability, by contrast, IS handled by the retry path below.)
	if err := postgres.ValidateMigrationDSN(cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}

	// Fast path: one synchronous attempt. On success, wire everything now.
	pool, initErr := initDatabase(context.Background(), cfg.DatabaseURL)
	switch {
	case initErr == nil:
		c.setPool(pool)
		c.wireLiveBackends(pool, enc, cfg)
		if host := cfg.RedactedDatabaseHost(); host != "" {
			slog.Info("dependency container initialized", "database", host)
		} else {
			slog.Info("dependency container initialized")
		}
		return c, nil
	case postgres.IsPermanentMigrationErr(initErr):
		// A schema the pod cannot serve against — a missing or invalid constraint-bearing
		// index (migrations run in the PreSync Job now, so boot only VERIFIES) — can NEVER
		// be cleared by retrying; it needs an operator to run the rebuild DDL the error
		// carries. Fail fast so the failure is loud (pod crash) rather than a silent 503
		// loop that burns the startup-probe budget and then restarts to the same state.
		return nil, fmt.Errorf("database schema is in a permanent-failure state (needs manual recovery): %w", initErr)
	default:
		slog.Warn("database not ready at startup; booting in 503 mode and retrying in the background",
			"error", initErr.Error())
	}

	// Transient failure: boot in 503 mode. Wire the health dependency to notReady
	// (so /readyz reports 503, unlike the no-database mode), the connection service
	// with a nil repo, and the brief service with nil repos (its routes stay mounted
	// and return the typed 503). The background goroutine late-binds the live
	// pool/repos into ALL THREE (connection, brief, health readiness) once it opens,
	// so brief + job routes go live without a pod restart.
	campaign := service.NewCampaignService(notReady{})
	connections := c.newConnectionService(nil, enc)
	briefs := c.newBriefService(nil, nil, nil, nil)
	auds := c.newAudienceService(nil, nil)
	c.Service = campaign
	c.Connections = connections
	c.Briefs = briefs
	c.Audiences = auds

	initCtx, cancel := context.WithCancel(context.Background())
	c.cancelInit = cancel
	c.initDone = make(chan struct{})
	go c.retryDatabaseInit(initCtx, cfg, enc, campaign, connections, briefs, auds)

	return c, nil
}

// wireLiveBackends wires all services against a live pool: the connection service
// (repo + encryptor), the brief service and its async orchestrator (brief/campaign/
// job repos; no dispatchers registered yet, so a startup warning notes campaigns
// record jobs but perform no upstream dispatch), and the campaign/health service so
// /readyz reflects DB connectivity. It also recovers jobs orphaned by a prior pod
// restart and starts the periodic recovery sweeper. Shared by the fast path (and,
// once brief/orchestrator late-binding exists, reusable from the retry path).

// registerDispatchers builds the per-provider PlatformDispatcher map from the
// connection repo + encryptor. Adapters resolve+decrypt each project's connection
// themselves. Called from BOTH the fast path and the cold-start retry path so the
// registered set stays identical regardless of how the DB comes up. Add a provider
// here as its adapter lands; a provider with no entry records jobs that report "no
// dispatcher registered" for that platform (logged as a startup warning).
// audiences is the audience repository the HubSpot (email) dispatcher reads to resolve a brief's
// built send-list. The ad dispatchers don't need it, so it is a distinct arg rather than folded
// into the connection repo.
func registerDispatchers(repo *postgres.ConnectionRepo, enc domain.Encryptor, audiences *postgres.AudienceRepo) map[model.Provider]service.PlatformDispatcher {
	return map[model.Provider]service.PlatformDispatcher{
		model.ProviderRedditAds:    dispatch.NewRedditDispatcher(repo, enc),
		model.ProviderLinkedInAds:  dispatch.NewLinkedInDispatcher(repo, enc),
		model.ProviderMetaAds:      dispatch.NewMetaDispatcher(repo, enc),
		model.ProviderTwitterAds:   dispatch.NewTwitterDispatcher(repo, enc),
		model.ProviderGoogleAds:    dispatch.NewGoogleAdsDispatcher(repo, enc),
		model.ProviderHubSpot:      dispatch.NewHubSpotDispatcher(repo, enc, audiences),
		model.ProviderMicrosoftAds: dispatch.NewMicrosoftDispatcher(repo, enc),
	}
}

// dispatchableProviders is the full set of providers a brief can select (per the
// CreateCampaigns contract); any without a registered dispatcher is logged at startup so the
// gap is visible in production.
//
// Named for what it gates — DISPATCH — not "ad platforms". It deliberately spans BOTH channel
// kinds (see model.ChannelKind): the six paid ad platforms AND the hubspot email channel.
// Email is dispatchable (it stages a draft) even though it is not an ad platform and has no
// run state to pause, so folding it under an "adPlatform" name misdescribed it.
var dispatchableProviders = []model.Provider{
	model.ProviderGoogleAds, model.ProviderLinkedInAds, model.ProviderMetaAds,
	model.ProviderRedditAds, model.ProviderTwitterAds, model.ProviderMicrosoftAds,
	model.ProviderHubSpot,
}

// newAudienceBuilder builds the platform-side audience builder from config.
//
// Snowflake is OPTIONAL and configured as a GROUP: with account/user/key not all present the
// warehouse is treated as unconfigured and the builder resolves no past editions, so an
// audience is still built from the event's own country and records the narrower scope. A
// warehouse misconfiguration must not block the email channel — enriching an audience with
// past editions is an enrichment, not a correctness requirement.
//
// HubSpot needs no config here: its credentials are per-project encrypted connections the
// builder resolves per call, exactly as the dispatchers do.
//
// Returns (builder, snowflakeClient). The snowflakeClient (if non-nil) owns database
// sessions and must be closed during Container.Close to release those resources.
func newAudienceBuilder(repo *postgres.ConnectionRepo, enc domain.Encryptor, cfg *config.Config) (service.AudienceBuilder, *snowflake.Client) {
	var snow dispatch.PastEditionResolver
	var client *snowflake.Client
	if cfg.SnowflakeAccount != "" && cfg.SnowflakeUser != "" && cfg.SnowflakePrivateKey != "" {
		var err error
		client, err = snowflake.NewClient(snowflake.Config{
			Account:       cfg.SnowflakeAccount,
			User:          cfg.SnowflakeUser,
			PrivateKeyPEM: cfg.SnowflakePrivateKey,
			Warehouse:     cfg.SnowflakeWarehouse,
			Role:          cfg.SnowflakeRole,
		})
		if err != nil {
			// Log and continue: a bad key is a config error, but failing boot over it would
			// take down campaign dispatch for a read-only enrichment.
			//
			// The error is CARRIED, not just logged. Dropping it here (as this originally did)
			// left a nil resolver, which ResolvePastEditions reports as (nil, nil) — the same
			// answer as "no warehouse configured". A returning KubeCon would then lose its
			// entire past-registrant audience while the build reported success and stored the
			// benign first-time-event note. This boot log rotates away; the audience row does not.
			slog.Warn("snowflake is configured but unusable; audiences will be built country-only",
				"error", err)
			return dispatch.NewDegradedAudienceBuilder(repo, enc, err), nil
		}
		snow = client
	} else {
		slog.Info("snowflake not configured; audiences will be built from the event's country only")
	}
	return dispatch.NewAudienceBuilder(repo, enc, snow), client
}

// newConnectionService constructs a ConnectionService with the shared token verifier
// injected. Same one-helper rule as newBriefService, for the reason given on
// Container.tokenVerifier: three call sites construct this service, and one that skipped
// the verifier would reject every request to the connection routes.
func (c *Container) newConnectionService(repo domain.ConnectionRepository, enc domain.Encryptor) *service.ConnectionService {
	s := service.NewConnectionService(repo, enc)
	s.SetTokenVerifier(c.tokenVerifier)
	return s
}

// newAudienceService constructs an AudienceService with the audience-build dependencies
// injected. EVERY construction in this file goes through it: the builder is opt-in via
// SetBuilder, so a path that constructs the service directly still compiles and serves — the
// build endpoint would just return 503 forever, silently.
//
// A nil audienceBuilder is normal, not a failure: it means HubSpot/Snowflake are unconfigured,
// and BuildAudience then returns the contract's typed 503 while the CRUD routes stay usable.
func (c *Container) newAudienceService(repo domain.AudienceRepository, briefs domain.BriefRepository) *service.AudienceService {
	s := service.NewAudienceService(repo)
	s.SetTokenVerifier(c.tokenVerifier)
	if briefs != nil {
		s.SetBriefRepo(briefs)
	}
	if c.audienceBuilder != nil {
		s.SetBuilder(c.audienceBuilder)
	}
	return s
}

// stuckClaimScanTimeout bounds the startup stuck-claim scan so a slow or unavailable database
// cannot delay readiness. The scan is diagnostic only, so timing out is survivable.
const stuckClaimScanTimeout = 5 * time.Second

// maxStuckClaimDetailLogs caps the per-row detail lines. The realistic bad day is exactly the
// one this diagnostic exists for — a rolling deploy stranding many claims — so an uncapped
// loop would bury the startup log in the moment an operator most needs to read it. The
// summary line reports how many rows were elided.
const maxStuckClaimDetailLogs = 10

// logStuckDispatchClaims reports 'pending' dispatch claims stranded by a previous process at
// STARTUP — the moment they are most likely to exist, since the usual cause is a pod dying
// mid-dispatch (a crash, an OOM kill, or an eviction during a rolling deploy).
//
// Without this the rows are INVISIBLE: the claim is ON CONFLICT (brief_id, platform), so a
// stranded row silently blocks every future dispatch for that pair, and an operator finds out
// only when someone reports a campaign that will not dispatch. Nothing here reclaims or
// deletes — see stuckClaimReportAge for why a time-based takeover would be unsafe — the point is to
// turn a silent block into an alertable log line.
//
// Runs inline on the wiring path (one bounded query) rather than as a background goroutine.
// To be precise about what that does and does not buy: it does NOT avoid duplication — every
// replica boots and scans, so a rolling deploy logs the same stuck row once per replica. What
// it does buy is BOUNDED duplication: startup-only rather than every sweep interval forever.
// (StartRecoverySweeper already runs on all replicas without leader election, so a goroutine
// here would not have been unprecedented — just noisier.)
//
// The per-row lines are therefore kept few and the SUMMARY is the line to alert on: it is
// low-cardinality and identical across replicas, so an alert built on it dedups naturally. A
// gauge would be better still, but this service exposes no metrics endpoint today.
func logStuckDispatchClaims(repo stuckClaimScanner) {
	scanStuckDispatchClaims(context.Background(), repo, "at startup")
}

// scanStuckDispatchClaims performs ONE bounded scan and logs what it finds. phase names the
// caller ("at startup" / "by periodic sweep") so an operator can tell a claim stranded by the
// deploy that just happened from one the running process has been watching.
func scanStuckDispatchClaims(parent context.Context, repo stuckClaimScanner, phase string) {
	ctx, cancel := context.WithTimeout(parent, stuckClaimScanTimeout)
	defer cancel()

	// 0 = the repo's defaultStuckClaimLimit (100).
	stuck, err := repo.StuckDispatchClaims(ctx, 0)
	if err != nil {
		// Diagnostic only — never block startup on it.
		slog.WarnContext(ctx, "could not scan for stuck dispatch claims", "phase", phase, "error", err)
		return
	}
	if len(stuck) == 0 {
		return
	}
	// Summary FIRST: it is the line an operator alerts on, so it must not sit beneath the
	// detail it summarizes. "truncated" distinguishes "exactly N stuck" from "at least N" —
	// a saturated limit means the real number is unknown and larger.
	// The repo queries limit+1, so more rows than the cap means the true total is unknown and
	// larger — report that rather than a flat count that understates an incident.
	truncated := len(stuck) > postgres.DefaultStuckClaimLimit
	if truncated {
		stuck = stuck[:postgres.DefaultStuckClaimLimit]
	}
	slog.WarnContext(ctx, "stuck dispatch claims detected "+phase,
		"count", len(stuck), "truncated", truncated,
		"reported", min(len(stuck), maxStuckClaimDetailLogs),
		"runbook", "docs/knowledge/code/internal-dispatch.md")
	for i, c := range stuck {
		if i >= maxStuckClaimDetailLogs {
			break
		}
		// A short, stable message so operators can group and alert on it; the remediation
		// procedure lives in the runbook referenced above, not in the message field.
		//
		// created_at AND updated_at are both emitted because they mean different things here.
		// created_at is the CLAIM time and is never rewritten (UpsertCampaign updates
		// updated_at only), so for a row the orchestrator later upserted with an ambiguous
		// outcome, created_at alone would make a days-old unreconciled row look like a fresh
		// crash. version > 1 is the in-band signal that such an upsert happened — i.e. this is
		// an ambiguous outcome where a paid campaign MAY exist upstream (verify before
		// deleting), not a bare abandoned claim (usually safe to delete).
		slog.WarnContext(ctx, "stuck dispatch claim",
			"project_id", c.ProjectID, "brief_id", c.BriefID, "platform", c.Platform,
			"job_id", c.JobID, "created_at", c.CreatedAt, "updated_at", c.UpdatedAt,
			"version", c.Version, "upserted_after_claim", c.Version > 1,
			// EXPLICIT rather than inferred from upserted_after_claim=false: a bare claim is
			// only safe to delete once you know no dispatch is still running for it. Reading
			// "false" as "safe" would be wrong for a claim whose dispatch is in flight right
			// now, which looks identical in this row. Never "safe", only "needs verification"
			// vs "MAY have created a paid campaign upstream".
			"remediation", stuckClaimRemediation(c),
			"platform_campaign_id", c.PlatformCampaignID, "has_result", len(c.Result) > 0)
	}
}

// stuckClaimRemediation describes what an operator must verify before touching a stuck claim.
// It deliberately never says "safe to delete", and it ALWAYS requires checking the upstream
// platform.
//
// An earlier version gave a bare version-1 row the weaker "verify no dispatch is in flight"
// on the theory that no upsert had happened, so nothing could exist upstream. That inference
// is WRONG, and it is wrong on exactly the paths this diagnostic exists to surface.
// orchestrator.dispatchOne RETAINS the claim WITHOUT upserting when a dispatcher returns
// (nil, nil), when it returns an empty upstream id, or when it returns (nil, err) that is not
// pre-create — and its own comments state that none of those prove the provider did not create
// a campaign. All three leave a row that is byte-for-byte identical to an abandoned pre-create
// claim: version 1, no platform id, no result blob.
//
// So the weaker guidance was satisfiable (the worker is gone) on a row whose paid campaign may
// be live, which would authorize a duplicate create — the precise failure the claim exists to
// prevent. The schema cannot separate the two cases, so the honest floor is upstream
// verification for every row; version/id/result only sharpen WHY it is owed. The runbook
// carries the procedure.
func stuckClaimRemediation(c *model.Campaign) string {
	if c.Version > 1 || c.PlatformCampaignID != "" || len(c.Result) > 0 {
		return "verify upstream platform before deleting: a paid campaign may exist (dispatch recorded a partial or ambiguous result)"
	}
	// Deliberately the same REQUIREMENT as above, differing only in why it is owed.
	return "verify upstream platform before deleting: a paid campaign may exist (a retained claim does not prove the provider was never called)"
}

// stuckClaimScanner is the one method the stuck-claim scan needs. Declared as an
// interface (rather than taking *postgres.CampaignRepo) so the sweeper's re-scan
// behaviour is testable without a live database — the property under test is that a
// SECOND scan happens at all, which a startup-only implementation would never do.
type stuckClaimScanner interface {
	StuckDispatchClaims(ctx context.Context, limit int) ([]*model.Campaign, error)
}

// stuckClaimSweepInterval is how often the background sweeper re-scans for stranded
// dispatch claims. Matches the orchestrator's recoverySweepInterval: the two solve the
// same problem (a startup-only scan cannot see work stranded AFTER this pod booted) and
// a shared cadence keeps the operational picture uniform.
// A var, not a const, only so tests can shrink it; production never changes it.
var stuckClaimSweepInterval = 5 * time.Minute

// startStuckClaimSweeper re-runs the stuck-claim scan periodically.
//
// The startup scan alone is NOT sufficient, and the gap is exactly the common case: a claim
// stranded seconds before a rolling deploy or crash-restart is YOUNGER than
// stuckClaimReportAge (4m), so the replacement pod's boot scan skips it — and without a
// periodic re-scan nothing ever looks again, leaving the row silently blocking every future
// dispatch for its (brief_id, platform). This is the same reasoning that gave
// Orchestrator.StartRecoverySweeper its sweep, applied to claims instead of jobs.
//
// Still REPORT-ONLY: nothing here reclaims or deletes (see stuckClaimReportAge for why a
// time-based takeover would be unsafe — 'pending' cannot distinguish a claim in flight from
// an ambiguous outcome where a paid campaign may already exist upstream).
//
// Like the startup scan this runs on every replica without leader election, so a stuck row is
// logged once per replica per interval. That is accepted for the same reason: the summary line
// is low-cardinality and identical across replicas, so an alert built on it dedups naturally.
func (c *Container) startStuckClaimSweeper(repo stuckClaimScanner) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.cancelSweep = cancel
	c.sweepDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(stuckClaimSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scanStuckDispatchClaims(ctx, repo, "by periodic sweep")
			}
		}
	}()
}

// logMissingDispatchers warns about providers that have no adapter yet — those channels
// record jobs that finish "failed" with "no dispatcher registered".
//
// The channel KIND is logged alongside each provider so an operator can tell a missing PAID
// platform (no campaigns will run, budget is unspent) from a missing EMAIL channel (no drafts
// are staged) — different urgency, different remediation.

func logMissingDispatchers(dispatchers map[model.Provider]service.PlatformDispatcher) {
	// Split by KIND into separate structured fields rather than formatting the kind into each
	// string. An operator filtering for "which paid platforms are down" can then match a field
	// instead of substring-matching "(paid-ads)" — the same brittle string-matching this
	// change argues against elsewhere. It also keeps the test asserting on data, not on a
	// format string.
	var missingPaid, missingEmail []string
	for _, p := range dispatchableProviders {
		if _, ok := dispatchers[p]; ok {
			continue
		}
		if p.IsPaidAds() {
			missingPaid = append(missingPaid, string(p))
			continue
		}
		missingEmail = append(missingEmail, string(p))
	}
	if len(missingPaid) == 0 && len(missingEmail) == 0 {
		return
	}
	slog.Warn("some channels have no dispatcher registered; campaigns for them record jobs but perform no upstream dispatch",
		"missing_paid_ads", missingPaid, "missing_email", missingEmail,
		"registered", len(dispatchers))
}

func (c *Container) wireLiveBackends(pool *postgres.Pool, enc domain.Encryptor, cfg *config.Config) {
	repo := postgres.NewConnectionRepo(pool)
	c.Connections = c.newConnectionService(repo, enc)
	briefRepo := postgres.NewBriefRepo(pool)
	campaignRepo := postgres.NewCampaignRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)
	// Register the per-provider dispatchers. Providers WITHOUT an adapter yet record
	// jobs that report "no dispatcher registered" for that platform (the remaining
	// ad-platform + email adapters land incrementally, LFXV2-2636..2642 / 2777);
	// warn so that gap is visible in production logs.
	audienceRepo := postgres.NewAudienceRepo(pool)
	// Must precede newAudienceService below, which reads c.audienceBuilder.
	c.audienceBuilder, c.snowflakeClient = newAudienceBuilder(repo, enc, cfg)
	dispatchers := registerDispatchers(repo, enc, audienceRepo)
	logMissingDispatchers(dispatchers)
	// Surface claims stranded by a previous process (crash/eviction mid-dispatch) — they
	// silently block future dispatches for their (brief, platform) until a human acts.
	logStuckDispatchClaims(campaignRepo)
	// The startup scan can't see a claim stranded younger than the report age (the
	// rolling-deploy case); a periodic sweep catches those. Stopped by Close.
	c.startStuckClaimSweeper(campaignRepo)
	orch := c.newOrchestrator(campaignRepo, jobRepo, dispatchers)
	c.orch = orch
	// Inject the orchestrator into the connection service for account-listing operations.
	// Through backendSetter, not a *service.ConnectionService cast: the cold-start path
	// below binds through that same interface, so both injection sites are held to one
	// declared contract and a signature change breaks both at compile time rather than
	// leaving this one silently behind.
	c.Connections.(backendSetter).SetOrchestrator(orch)
	c.Briefs = c.newBriefService(briefRepo, campaignRepo, jobRepo, orch)
	c.Audiences = c.newAudienceService(audienceRepo, briefRepo)

	// Recover jobs orphaned by a previous pod's restart: a queued/running job's
	// dispatch goroutine lived only in that process, so fail them forward now
	// rather than leaving them non-terminal forever. Bounded by the startup
	// deadline; a failure here is logged but not fatal (the service can still run).
	recoverCtx, cancel := context.WithTimeout(context.Background(), startupDBTimeout)
	defer cancel()
	if n, rerr := jobRepo.FailStuckJobs(recoverCtx, "job did not complete before a service restart"); rerr != nil {
		slog.Warn("failed to recover stuck jobs on startup", "error", rerr)
	} else if n > 0 {
		slog.Info("recovered stuck jobs on startup", "count", n)
	}

	// The startup scan can't recover a job orphaned by a crash younger than the
	// stale cutoff (too new to look stuck at boot, never re-examined). A periodic
	// sweep catches those; it stops on Shutdown via the orchestrator's root ctx.
	orch.StartRecoverySweeper()

	// Drain the index outbox: rows co-committed with their resource whose publish never
	// landed. Without this a dropped message is lost, and a terminal write (archiving a
	// brief) has no later write to repair the index.
	c.setIndexRelay(indexer.NewRelay(postgres.NewOutboxRepo(pool), c.rawPublisher(), cfg.IndexerServiceToken))

	// The health service's readiness depends on the database pool (Readyz).
	c.Service = service.NewCampaignService(pool)
}

// retryDatabaseInit keeps attempting migration+pool-open until it succeeds or the
// context is cancelled (shutdown). On success it late-binds the live pool into ALL
// the mounted services — connection, brief (repos + orchestrator), and health
// readiness — and runs the same stuck-job recovery + periodic sweeper the fast path
// does, so /readyz flips to healthy AND the connection + brief/job routes go live
// without a pod restart.
func (c *Container) retryDatabaseInit(ctx context.Context, cfg *config.Config, enc domain.Encryptor, r readinessSetter, b backendSetter, bb briefBackendSetter, ab audienceBackendSetter) {
	defer close(c.initDone)

	for attempt := 1; ; attempt++ {
		pool, err := initDatabase(ctx, cfg.DatabaseURL)
		if err == nil {
			c.setPool(pool)
			// Late-bind the connection service.
			connRepo := postgres.NewConnectionRepo(pool)
			b.SetBackend(connRepo, enc)
			// Late-bind the brief service (repos + orchestrator) so brief/job routes go
			// live, and run the stuck-job recovery + start the periodic sweeper — the
			// same work wireLiveBackends does on the fast path.
			briefRepo := postgres.NewBriefRepo(pool)
			campaignRepo := postgres.NewCampaignRepo(pool)
			jobRepo := postgres.NewJobRepo(pool)
			// Same dispatcher set as the fast path (see registerDispatchers).
			audienceRepo := postgres.NewAudienceRepo(pool)
			c.audienceBuilder, c.snowflakeClient = newAudienceBuilder(connRepo, enc, cfg)
			dispatchers := registerDispatchers(connRepo, enc, audienceRepo)
			logMissingDispatchers(dispatchers)
			// Same stuck-claim scan as the fast path: the DB only just became reachable, so
			// this is the first opportunity to see claims stranded by a previous process.
			//
			// Derived from ctx (the init context Close cancels), NOT context.Background():
			// if shutdown begins while this scan is blocked in the DB, cancelling ctx
			// interrupts the statement so Close's <-c.initDone wait can't overrun the
			// bounded shutdown budget by up to stuckClaimScanTimeout. Same reasoning as the
			// FailStuckJobs call below.
			scanStuckDispatchClaims(ctx, campaignRepo, "at startup")
			// Start the periodic sweep here too. Safe without a lock for the same reason
			// as c.orch below: Close waits on <-c.initDone before reading these fields.
			c.startStuckClaimSweeper(campaignRepo)
			orch := c.newOrchestrator(campaignRepo, jobRepo, dispatchers)
			// Safe without a lock: Close() waits on <-c.initDone (closed when this
			// goroutine returns) before it reads c.orch, so this write happens-before
			// that read.
			c.orch = orch
			bb.SetBackend(briefRepo, campaignRepo, jobRepo, orch)
			ab.SetBackend(audienceRepo)
			// Inject the orchestrator into the connection service for account-listing operations.
			b.SetOrchestrator(orch)
			// The audience service was constructed in 503 mode with no brief repo and no
			// builder, so binding only the repo leaves BuildAudience permanently 503 on this
			// path. Bind the other two here (the builder was created just above).
			ab.SetBriefRepo(briefRepo)
			ab.SetBuilder(c.audienceBuilder)
			// Derive from ctx (the init context Close cancels), NOT context.Background():
			// if shutdown begins while FailStuckJobs is blocked on the DB, cancelling
			// ctx interrupts the statement so Close's <-c.initDone wait can't overrun the
			// bounded shutdown budget by up to startupDBTimeout. The timeout still bounds a
			// slow query during normal startup.
			recoverCtx, cancelRecover := context.WithTimeout(ctx, startupDBTimeout)
			if n, rerr := jobRepo.FailStuckJobs(recoverCtx, "job did not complete before a service restart"); rerr != nil {
				slog.Warn("failed to recover stuck jobs on startup", "error", rerr)
			} else if n > 0 {
				slog.Info("recovered stuck jobs on startup", "count", n)
			}
			cancelRecover()
			orch.StartRecoverySweeper()

			// Same relay as the fast path (see wireLiveBackends). Written under c.mu: this runs
			// on the init goroutine while Close may be reading the field concurrently.
			c.setIndexRelay(indexer.NewRelay(postgres.NewOutboxRepo(pool), c.rawPublisher(), cfg.IndexerServiceToken))
			// Flip readiness LAST, so /readyz only reports healthy after the brief
			// service is fully wired (avoids a window where /readyz is OK but brief
			// routes still 503).
			r.SetReadinessDep(pool)
			if host := cfg.RedactedDatabaseHost(); host != "" {
				slog.Info("database now ready; connection + brief endpoints live", "database", host, "attempts", attempt)
			} else {
				slog.Info("database now ready; connection + brief endpoints live", "attempts", attempt)
			}
			return
		}
		// A permanent schema-verification failure (a missing or invalid required index)
		// will never clear by retrying, so stop the loop and surface it loudly. /readyz
		// stays at 503 with no live pool, but the ERROR log makes the reason unambiguous
		// instead of an endless silent "will retry" stream — an operator must run the
		// rebuild DDL the error carries.
		if postgres.IsPermanentMigrationErr(err) {
			slog.Error("background database initialization hit a permanent schema-verification failure (needs manual recovery); stopping retries",
				"attempt", attempt, "error", err.Error())
			return
		}
		slog.Warn("background database initialization attempt failed; will retry",
			"attempt", attempt, "retryIn", dbRetryInterval.String(), "error", err.Error())

		select {
		case <-ctx.Done():
			slog.Info("stopping background database initialization (shutdown)")
			return
		case <-time.After(dbRetryInterval):
		}
	}
}

// setPool stores the live pool under the lock (Close reads it on shutdown).
func (c *Container) setPool(pool *postgres.Pool) {
	c.mu.Lock()
	c.pool = pool
	c.mu.Unlock()
}

// initDatabase verifies the schema and opens the pool within a single bounded attempt.
// Schema MUTATION no longer happens here: migrations run in the `migrate` subcommand as an
// ArgoCD PreSync Job, before the rollout. Boot only VERIFIES the schema (required/invalid
// indexes) and fails closed if a constraint-bearing index is missing or invalid — the guard
// /readyz relies on — so the previous release is never migrated out from under while it may
// still be serving. Returns the live pool or an error.
func initDatabase(parent context.Context, dsn string) (*postgres.Pool, error) {
	ctx, cancel := context.WithTimeout(parent, startupDBTimeout)
	defer cancel()

	// Open the pool FIRST: NewPool does a context-bounded Ping (pool.go), so when the
	// database is unreachable this fails fast within the deadline rather than blocking.
	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}

	// VerifySchema is a bounded catalog read (no Up(), no context argument), so run it under
	// the startup deadline via a goroutine and return on ctx: a hung read must not wedge boot.
	// Unlike the old migration path it needs no serialization — it is read-only and idempotent,
	// so overlapping boot retries are harmless.
	verifyDone := make(chan error, 1)
	go func() { verifyDone <- postgres.VerifySchema(dsn) }()
	select {
	case vErr := <-verifyDone:
		if vErr != nil {
			pool.Close()
			return nil, fmt.Errorf("verify schema: %w", vErr)
		}
	case <-ctx.Done():
		pool.Close()
		return nil, fmt.Errorf("verify schema: %w", ctx.Err())
	}
	// A successful verifyDone can win the select even if ctx was ALSO cancelled (Go picks a
	// ready case pseudo-randomly when both fire together). Returning a live pool here would let
	// retryDatabaseInit late-bind backends, start the sweeper, and flip readiness AFTER Close
	// cancelled init — the exact pool swap Close means to prevent. Re-check and fail closed.
	if err := ctx.Err(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("verify schema: %w", err)
	}
	return pool, nil
}

// Close releases any resources held by the container. It first stops the background
// DB-init goroutine (if a cold start is still retrying), then stops the periodic
// stuck-claim sweeper, then drains in-flight campaign dispatch so a dispatch that
// already created an upstream campaign isn't cut off before it persists, THEN closes
// the database pool.
//
// The stuck-claim sweeper stop (sweeperStopTimeout) is waited on HERE, by Close, before
// Shutdown is called — Orchestrator.Shutdown owns only two separately-budgeted phases: a
// clean dispatch drain (dispatchDrainTimeout), then (only if that elapses) a post-cancel
// grace (CancelGracePeriod). (Shutdown does cancel its own periodic recovery sweeper, but
// that is an unbudgeted cancel, not a wait.) All of these must fit within ctx, so ctx MUST
// carry the full ContainerCloseTimeout (= sweeperStopTimeout + relayStopTimeout +
// dispatchDrainTimeout + CancelGracePeriod + indexer.DrainTimeout), not just the drain
// timeout — otherwise the grace phase would have zero budget and the pool could close
// while a just-cancelled dispatch is still finalizing job/campaign state.
// setIndexRelay installs and starts a relay under c.mu. The 503 cold-start path assigns from the
// init goroutine while Close may read concurrently, so the field is mutex-guarded like c.pool.
func (c *Container) setIndexRelay(relay *indexer.Relay) {
	c.mu.Lock()
	c.indexRelay = relay
	c.mu.Unlock()
	relay.Start()
}

// getIndexRelay reads the relay under c.mu. Callers must have joined the init goroutine first if
// they need the FINAL value (see Close).
func (c *Container) getIndexRelay() *indexer.Relay {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.indexRelay
}

func (c *Container) Close(ctx context.Context) error {
	// Stop the background DB-init goroutine first (if the container booted in 503
	// mode and is still retrying), and wait for it to exit so it can't open/swap a
	// pool after we've decided to shut down.
	// Join the DB-init goroutine BEFORE touching the relay. It is the goroutine that installs
	// a relay on the 503 cold-start path, so reading the field first loses the race: a retry
	// that succeeds just after the read starts a relay nothing then stops, and it would go on
	// reading the outbox through the pool.Close below.
	if c.cancelInit != nil {
		c.cancelInit()
		<-c.initDone
	}
	// Now the relay set is final. Stop it before the pool: it reads the outbox, so an in-flight
	// pass must not outlive the pool. Bounded — abandoning a pass is safe because unpublished
	// rows stay pending and the next process drains them, which is exactly what the outbox is for.
	if relay := c.getIndexRelay(); relay != nil {
		relay.Stop(relayStopTimeout)
	}
	// Stop the periodic stuck-claim sweeper. This MUST come after the <-c.initDone wait
	// above: on the cold-start path the retry goroutine is what assigns cancelSweep, so
	// reading it earlier would be an unsynchronized read that could also miss a sweeper
	// started moments later. Waiting first means the assignment happens-before this read.
	//
	// The wait is bounded so a wedged sweeper cannot spend the dispatch drain's budget on a
	// diagnostic. But giving up on the wait is NOT sufficient on its own: the scan runs a
	// pgxpool.Query, which holds a pooled CONNECTION until its rows are closed, and
	// pgxpool.Close "blocks until all connections are returned to pool and closed" — so an
	// abandoned sweeper still stalls pool.Close() below, just later and less visibly.
	//
	// Cancelling sweeperCtx is what actually releases the connection: pgx aborts the
	// in-flight statement on context cancellation and returns the connection to the pool.
	// The wait below therefore exists to give that release a moment to complete in the
	// common case; the timeout only stops us blocking on a driver that is slow to unwind,
	// and the release still happens before pool.Close() can finish.
	if c.cancelSweep != nil {
		c.cancelSweep()
		select {
		case <-c.sweepDone:
		case <-time.After(sweeperStopTimeout):
			// Not merely cosmetic: if this fires, pool.Close() below will wait for the same
			// connection, so the delay is deferred rather than avoided. Log it so a shutdown
			// that overruns its budget is attributable rather than mysterious.
			slog.Warn("stuck-claim sweeper did not stop promptly; pool close may block until its scan unwinds",
				"timeout", sweeperStopTimeout)
		}
	}
	// Capture the orchestrator shutdown error but do NOT early-return on it: the
	// pool must still be closed even if the drain timed out with dispatches still
	// running. Returning the error (rather than swallowing it) makes a shutdown
	// failure — dispatches still running when the pool was closed — observable to
	// the caller's "container close error" branch and its logs.
	var shutdownErr error
	if c.orch != nil {
		if err := c.orch.Shutdown(ctx, dispatchDrainTimeout); err != nil {
			slog.Warn("timed out draining in-flight dispatch on shutdown", "error", err)
			shutdownErr = fmt.Errorf("drain in-flight dispatch: %w", err)
		}
	}
	// Cut short any in-flight UNCONFIRMED lock cooldown (postgres.ReleaseCampaignLockAfterCooldown)
	// before closing the pool: each one holds a checked-out connection for up to
	// unconfirmedLockCooldown (30s), and pgxpool.Close blocks until every checked-out
	// connection is returned, so an unsignalled cooldown would otherwise make pool.Close below
	// overrun the shutdown budget on its own. Same reasoning as the sweeper stop above.
	postgres.StopCooldownsForShutdown(cooldownStopTimeout)
	c.mu.Lock()
	pool := c.pool
	c.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
	// Close the Snowflake client AFTER the dispatch drain, as audience builds may still
	// be running and holding connections to the warehouse. Closing before dispatches finish
	// would abandon connections and sessions in the pool.
	if c.snowflakeClient != nil {
		if err := c.snowflakeClient.Close(); err != nil {
			slog.Warn("error closing snowflake client", "error", err)
			// Non-fatal: log it but don't override shutdownErr if the dispatch drain failed.
		}
	}
	// Close the NATS connection AFTER the dispatch drain: a draining dispatch can
	// still persist a campaign and publish its index event, and closing earlier
	// would drop exactly those last writes from the search index.
	if c.indexPublisher != nil {
		c.indexPublisher.Close()
	}
	return shutdownErr
}

// newBriefService constructs a BriefService and injects the container's shared index
// publisher, LLM client (if configured), and event URL fetcher. EVERY BriefService construction
// in this file must go through it: the publisher is opt-in via SetIndexer (so the ~40 test call
// sites default to Noop), which means a path that calls service.NewBriefService directly
// compiles, runs, serves traffic and silently indexes NOTHING. Routing every path through one
// helper is what makes that failure impossible rather than merely unlikely.
func (c *Container) newBriefService(briefs domain.BriefRepository, campaigns domain.CampaignRepository, jobs domain.JobRepository, orch *service.Orchestrator) *service.BriefService {
	s := service.NewBriefService(briefs, campaigns, jobs, orch)
	s.SetTokenVerifier(c.tokenVerifier)
	s.SetIndexer(c.indexPublisher)
	s.SetEventURL(c.eventFetcher(), eventurl.NewParser())
	s.SetLLMClient(c.newLLMClient())
	if c.indexingDisabled() {
		s.DisableIndexing()
	}
	return s
}

// eventFetcher constructs the SSRF-guarded event-page fetcher for this deployment.
//
// The NAT64 prefixes are the whole reason this is a method and not a bare NewFetcher()
// call. eventurl decodes the well-known 64:ff9b::/96 unconditionally, but RFC 6052 §2.2
// network-specific prefixes are the operator's own global unicast space — undiscoverable
// in-process and indistinguishable from any other public prefix. On a cluster that uses
// one, an unlisted prefix is a live SSRF hole: the TRANSLATOR makes the IPv4 connection,
// so an encoded 169.254.169.254 satisfies every check inside this process. Config is the
// only place that fact can come from.
//
// WithNAT64Prefixes panics on a malformed or non-RFC-6052 length, and that is the wanted
// behaviour HERE specifically: this runs at composition, so a prefix typed wrong stops
// the pod from starting instead of silently decoding at the wrong offset for its lifetime.
func (c *Container) eventFetcher() *eventurl.Fetcher {
	if c.Config == nil || len(c.Config.EventURLNAT64Prefixes) == 0 {
		return eventurl.NewFetcher()
	}
	return eventurl.NewFetcher(eventurl.WithNAT64Prefixes(c.Config.EventURLNAT64Prefixes...))
}

// newLLMClient constructs the LLM client for email copy generation when configured.
// Returns nil when the proxy URL or API key is missing (email copy generation degrades
// to unavailable, not failing), matching the optional-GROUP pattern for Snowflake.
//
// A misconfiguration detected at construction (invalid URL format, empty credentials
// after trimming) logs a warning and returns nil rather than failing the pod: email
// copy generation is an enrichment, not a core platform feature, so degrading on
// misconfiguration is safer than crashing boot over a non-fatal setting.
func (c *Container) newLLMClient() *llm.Client {
	if c.Config == nil {
		return nil
	}
	if c.Config.AIProxyURL == "" || c.Config.AIAPIKey == "" {
		return nil
	}
	cfg := llm.Config{
		ProxyURL: c.Config.AIProxyURL,
		APIKey:   c.Config.AIAPIKey,
		Model:    c.Config.AIModel, // empty means llm.DefaultModel
	}
	client, err := llm.NewClient(cfg)
	if err != nil {
		// Log but don't fail: email generation is optional. The generation endpoint
		// will return 503 ServiceUnavailable.
		slog.Warn("llm client initialization failed; email copy generation will be unavailable",
			"error", err)
		return nil
	}
	return client
}

// indexingDisabled reports whether indexing is DELIBERATELY off, from CONFIG alone.
//
// Deliberately not `_, isNoop := c.indexPublisher.(indexer.Noop)`: newIndexPublisher also yields
// a Noop when the broker is merely UNREACHABLE at boot. Treating that as "disabled" would make a
// pod that started during a broker restart skip the outbox for its entire life — permanently
// losing those writes, since pending rows are never pruned and there is no reindex path. A
// transient outage must still enqueue and let the relay deliver on reconnect.
func (c *Container) indexingDisabled() bool {
	return c.Config == nil || strings.TrimSpace(c.Config.NATSUrl) == ""
}

// newOrchestrator constructs an Orchestrator with the container's shared index
// publisher injected. Campaign CREATES are persisted by the orchestrator (dispatchOne),
// not by BriefService, so an orchestrator without the publisher leaves every newly
// created campaign unsearchable until some later update republishes it. Same rationale
// as newBriefService: route EVERY construction through one helper so a path cannot
// silently keep the Noop.
func (c *Container) newOrchestrator(campaigns domain.CampaignRepository, jobs domain.JobRepository, dispatchers map[model.Provider]service.PlatformDispatcher) *service.Orchestrator {
	o := service.NewOrchestrator(campaigns, jobs, dispatchers)
	o.SetIndexer(c.indexPublisher)
	if c.indexingDisabled() {
		o.DisableIndexing()
	}
	return o
}

// rawPublisher exposes the index publisher's raw-publish capability for the relay. The publisher
// is always non-nil — a Noop stands in when indexing is disabled OR the broker was unreachable at
// boot — and Noop.PublishRaw reports FAILURE, so the relay leaves rows PENDING rather than
// retiring messages that were never sent. Retrying against a Noop is the correct outcome: the
// alternative silently drains the outbox into nothing.
func (c *Container) rawPublisher() indexer.RawPublisher {
	if rp, ok := c.indexPublisher.(indexer.RawPublisher); ok {
		return rp
	}
	return indexer.Noop{}
}

// newIndexPublisher builds the Query Service index publisher from config.
//
// A dial failure is logged, NOT fatal: the index is a read-side convenience served by another
// service, whereas campaign dispatch is this service's reason to exist. Refusing to boot over
// an unreachable broker would turn a degraded search experience into a total outage. An empty
// NATSUrl disables indexing outright and returns a Noop.
func newIndexPublisher(cfg *config.Config) (indexer.Publisher, error) {
	p, err := indexer.NewNATSPublisher(cfg.NATSUrl)
	if err != nil {
		// FATAL, not a warning. The publisher is built with RetryOnFailedConnect, so an
		// ordinary broker outage does NOT land here — it returns a reconnecting publisher that
		// heals itself. Reaching this branch means the configuration can never work (a
		// malformed URL, an unparseable scheme), and no retry will fix it.
		//
		// Carrying on with a Noop would be the worst of both worlds: NATS_URL is non-empty, so
		// every write still enqueues an outbox row, into a table this process can never drain
		// and whose pending rows are deliberately never pruned. The service would look healthy
		// while accumulating undeliverable work forever. Failing fast surfaces a config error
		// as a config error — matching how invalid database settings are already treated.
		return nil, fmt.Errorf("nats configuration: %w", err)
	}
	if _, isNoop := p.(indexer.Noop); isNoop {
		slog.Info("query-service indexing disabled (no NATS URL configured)")
	}
	return p, nil
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package container provides dependency injection for the application.
package container

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

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
}

// notReady is a ReadinessChecker that always reports not-ready. It is wired as
// the health dependency during a cold start so /readyz returns 503 (not OK)
// until the real pool is swapped in — distinct from the no-database mode, where
// no dependency is wired and /readyz reports ready.
type notReady struct{}

func (notReady) Ready(context.Context) bool { return false }

// dispatchDrainTimeout bounds how long Container.Close waits for in-flight
// campaign dispatch to finish before the pool is closed. Together with the
// orchestrator's post-cancel grace (service.CancelGracePeriod) it forms
// ContainerCloseTimeout, which is reserved out of the overall graceful-shutdown
// budget (constants.DefaultShutdownTimeout) so the HTTP-drain phase and this
// phase can't sum past it and get SIGKILLed. Validated by the init() below.
//
// Sized so dispatchDrainTimeout + CancelGracePeriod leaves a positive HTTP-drain
// budget: CancelGracePeriod grew to cover the post-provider persist AND the
// terminal finalize write (both detached, both must complete during grace), so
// the drain window is trimmed to keep the total within DefaultShutdownTimeout.
const dispatchDrainTimeout = 6 * time.Second

// ContainerCloseTimeout is the wall-clock budget for Container.Close: the
// orchestrator drain (dispatchDrainTimeout) plus its post-cancel grace
// (service.CancelGracePeriod). The server budgets the HTTP-shutdown phase and
// this container-close phase separately (see HTTPShutdownTimeout), so the total
// graceful shutdown is a true sum bounded by constants.DefaultShutdownTimeout.
const ContainerCloseTimeout = dispatchDrainTimeout + service.CancelGracePeriod

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
	if dispatchDrainTimeout+service.CancelGracePeriod > constants.DefaultShutdownTimeout {
		panic("dispatchDrainTimeout + service.CancelGracePeriod exceeds DefaultShutdownTimeout")
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
func NewContainer(cfg *config.Config) (*Container, error) {
	slog.Info("initializing dependency container")

	if err := cfg.ValidateDatabaseSettings(); err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}

	c := &Container{Config: cfg}

	if cfg.DatabaseURL == "" {
		slog.Warn("database URL not set; connection and brief/campaign endpoints will return 503 Service Unavailable")
		c.Service = service.NewCampaignService(nil)
		// Wire the connection + brief services with nil repos so their routes are
		// still mounted and return the typed 503 ServiceUnavailable advertised by
		// the OpenAPI contract, rather than a bare 404 from unmounted routes.
		c.Connections = service.NewConnectionService(nil, nil)
		c.Briefs = service.NewBriefService(nil, nil, nil, nil)
		c.Audiences = service.NewAudienceService(nil)
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
		// A dirty schema (or other permanent migration state) can NEVER be cleared by
		// retrying — it needs an operator to inspect and force the version. Fail fast
		// so the failure is loud (pod crash) rather than a silent 503 loop that burns
		// the startup-probe budget and then restarts to the same broken state.
		return nil, fmt.Errorf("database migration is in a permanent-failure state (needs manual recovery): %w", initErr)
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
	connections := service.NewConnectionService(nil, enc)
	briefs := service.NewBriefService(nil, nil, nil, nil)
	auds := service.NewAudienceService(nil)
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
		model.ProviderRedditAds:   dispatch.NewRedditDispatcher(repo, enc),
		model.ProviderLinkedInAds: dispatch.NewLinkedInDispatcher(repo, enc),
		model.ProviderMetaAds:     dispatch.NewMetaDispatcher(repo, enc),
		model.ProviderTwitterAds:  dispatch.NewTwitterDispatcher(repo, enc),
		model.ProviderGoogleAds:   dispatch.NewGoogleAdsDispatcher(repo, enc),
		model.ProviderHubSpot:     dispatch.NewHubSpotDispatcher(repo, enc, audiences),
	}
}

// adPlatformProviders is the full set of providers a brief can select (per the
// CreateCampaigns contract); any without a registered dispatcher is logged at startup
// so the gap is visible in production. GoogleAds and MicrosoftAds are included even
// though their adapters aren't on main yet — CreateCampaigns accepts them, so a
// selection would otherwise fail silently with "no dispatcher registered" and never be
// surfaced here. (HubSpot is the email channel; its adapter now exists, LFXV2-2777.)
var adPlatformProviders = []model.Provider{
	model.ProviderGoogleAds, model.ProviderLinkedInAds, model.ProviderMetaAds,
	model.ProviderRedditAds, model.ProviderTwitterAds, model.ProviderMicrosoftAds,
	model.ProviderHubSpot,
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
// deletes — see staleClaimAge for why a time-based takeover would be unsafe — the point is to
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
// It deliberately never says "safe to delete": 'pending' cannot distinguish an abandoned claim
// from one whose dispatch is still running, so the only honest distinction is HOW MUCH
// verification is owed — check the platform for an orphaned campaign, or confirm no dispatch is
// in flight. The runbook carries the procedure.
func stuckClaimRemediation(c *model.Campaign) string {
	if c.Version > 1 || c.PlatformCampaignID != "" || len(c.Result) > 0 {
		return "verify upstream platform before deleting: a paid campaign may exist"
	}
	return "verify no dispatch is in flight before deleting"
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

// logMissingDispatchers warns about ad providers that have no adapter yet — those
// platforms record jobs that finish "failed" with "no dispatcher registered".
func logMissingDispatchers(dispatchers map[model.Provider]service.PlatformDispatcher) {
	var missing []string
	for _, p := range adPlatformProviders {
		if _, ok := dispatchers[p]; !ok {
			missing = append(missing, string(p))
		}
	}
	if len(missing) > 0 {
		slog.Warn("some ad platforms have no dispatcher registered; campaigns for them record jobs but perform no upstream dispatch",
			"missing", missing, "registered", len(dispatchers))
	}
}

func (c *Container) wireLiveBackends(pool *postgres.Pool, enc domain.Encryptor, cfg *config.Config) {
	repo := postgres.NewConnectionRepo(pool)
	c.Connections = service.NewConnectionService(repo, enc)
	briefRepo := postgres.NewBriefRepo(pool)
	campaignRepo := postgres.NewCampaignRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)
	// Register the per-provider dispatchers. Providers WITHOUT an adapter yet record
	// jobs that report "no dispatcher registered" for that platform (the remaining
	// ad-platform + email adapters land incrementally, LFXV2-2636..2642 / 2777);
	// warn so that gap is visible in production logs.
	audienceRepo := postgres.NewAudienceRepo(pool)
	dispatchers := registerDispatchers(repo, enc, audienceRepo)
	logMissingDispatchers(dispatchers)
	// Surface claims stranded by a previous process (crash/eviction mid-dispatch) — they
	// silently block future dispatches for their (brief, platform) until a human acts.
	logStuckDispatchClaims(campaignRepo)
	// The startup scan can't see a claim stranded younger than the report age (the
	// rolling-deploy case); a periodic sweep catches those. Stopped by Close.
	c.startStuckClaimSweeper(campaignRepo)
	orch := service.NewOrchestrator(campaignRepo, jobRepo, dispatchers)
	c.orch = orch
	c.Briefs = service.NewBriefService(briefRepo, campaignRepo, jobRepo, orch)
	c.Audiences = service.NewAudienceService(audienceRepo)

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
			orch := service.NewOrchestrator(campaignRepo, jobRepo, dispatchers)
			// Safe without a lock: Close() waits on <-c.initDone (closed when this
			// goroutine returns) before it reads c.orch, so this write happens-before
			// that read.
			c.orch = orch
			bb.SetBackend(briefRepo, campaignRepo, jobRepo, orch)
			ab.SetBackend(audienceRepo)
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
		// A permanent migration state (dirty schema) will never clear by retrying, so
		// stop the loop and surface it loudly. /readyz stays at 503 with no live pool,
		// but the ERROR log makes the reason unambiguous instead of an endless silent
		// "will retry" stream — an operator must force the migration version.
		if postgres.IsPermanentMigrationErr(err) {
			slog.Error("background database initialization hit a permanent migration failure (needs manual recovery); stopping retries",
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

// initDatabase runs migrations and opens the pool within a single bounded
// attempt. golang-migrate's Up() takes no context, so it is bounded by running
// it under the same deadline. Returns the live pool or an error.
func initDatabase(parent context.Context, dsn string) (*postgres.Pool, error) {
	ctx, cancel := context.WithTimeout(parent, startupDBTimeout)
	defer cancel()

	// Open the pool FIRST: NewPool does a context-bounded Ping (pool.go), so when the
	// database is unreachable this fails fast within the deadline. golang-migrate's
	// Up() takes no context and blocks until the DB responds, so running it against a
	// down database would hang past the deadline — and because the caller retries,
	// each hung attempt would leak another migration goroutine and race concurrent
	// migrations. Gating Migrate behind a successful (reachable) Ping ensures Migrate
	// only runs when the DB is actually up, where it connects immediately, so no
	// migration goroutine is ever left blocked and retries never overlap.
	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}

	// Migrate only after a reachable ping (above), so it connects immediately rather
	// than blocking against a down DB. It can still run long on a reachable DB if a
	// migration is slow or lock-blocked, and golang-migrate's Up() takes no context,
	// so bound it with the startup deadline: run it under migrateMu (only ONE
	// migration ever runs at a time, so a retry can't start a second while a prior is
	// still finishing) and return on the deadline. On timeout the in-flight migration
	// keeps running under the lock; the next retry blocks on migrateMu until it
	// finishes rather than launching an overlapping one.
	migrateDone := make(chan error, 1)
	go func() {
		migrateMu.Lock()
		defer migrateMu.Unlock()
		migrateDone <- postgres.Migrate(dsn)
	}()
	select {
	case mErr := <-migrateDone:
		if mErr != nil {
			pool.Close()
			return nil, fmt.Errorf("run migrations: %w", mErr)
		}
	case <-ctx.Done():
		pool.Close()
		return nil, fmt.Errorf("run migrations: %w", ctx.Err())
	}
	// A successful migrateDone can win the select even if ctx was ALSO cancelled
	// (Go picks a ready case pseudo-randomly when both fire together). Returning a
	// live pool here would let retryDatabaseInit late-bind backends, start the
	// sweeper, and flip readiness AFTER Close cancelled init — the exact pool swap
	// Close means to prevent. Re-check the context and fail closed if it's done.
	if err := ctx.Err(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return pool, nil
}

// migrateMu serializes golang-migrate runs so a retry never starts a second
// migration while a prior (possibly deadline-abandoned) one is still finishing.
var migrateMu sync.Mutex

// Close releases any resources held by the container. It first stops the background
// DB-init goroutine (if a cold start is still retrying), then drains in-flight
// campaign dispatch so a dispatch that already created an upstream campaign isn't
// cut off before it persists, THEN closes the database pool.
//
// Orchestrator.Shutdown runs two separately-budgeted phases: a clean drain
// bounded by dispatchDrainTimeout, then (only if that elapses) a post-cancel
// grace bounded by CancelGracePeriod. Both must fit within ctx, so ctx MUST
// carry the full ContainerCloseTimeout (= dispatchDrainTimeout +
// CancelGracePeriod), not just the drain timeout — otherwise the grace phase
// would have zero budget and the pool could close while a just-cancelled
// dispatch is still finalizing job/campaign state.
func (c *Container) Close(ctx context.Context) error {
	// Stop the background DB-init goroutine first (if the container booted in 503
	// mode and is still retrying), and wait for it to exit so it can't open/swap a
	// pool after we've decided to shut down.
	if c.cancelInit != nil {
		c.cancelInit()
		<-c.initDone
	}
	// Stop the periodic stuck-claim sweeper. This MUST come after the <-c.initDone wait
	// above: on the cold-start path the retry goroutine is what assigns cancelSweep, so
	// reading it earlier would be an unsynchronized read that could also miss a sweeper
	// started moments later. Waiting first means the assignment happens-before this read.
	// The sweeper still stops well before the pool closes below, and its own scans are
	// bounded by stuckClaimScanTimeout.
	if c.cancelSweep != nil {
		c.cancelSweep()
		<-c.sweepDone
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
	c.mu.Lock()
	pool := c.pool
	c.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
	return shutdownErr
}

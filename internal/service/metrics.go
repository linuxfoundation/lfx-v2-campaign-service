// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MetricsFetcher reads performance metrics for one campaign from its ad platform.
//
// Like StatusToggler, this is an OPTIONAL dispatcher capability rather than part of
// PlatformDispatcher: not every platform can report yet, so the sweeper type-asserts
// for it and treats a dispatcher that does not implement it as unsupported.
//
// An implementation MUST return domain.ErrMetricsUnsupported rather than an empty
// result when it cannot report. An empty-and-nil-error response is indistinguishable
// from "this campaign genuinely had no activity", which would overwrite real history
// with zeros.
type MetricsFetcher interface {
	// FetchMetrics returns daily rows for the inclusive range [from, to].
	FetchMetrics(ctx context.Context, campaign *model.Campaign, from, to time.Time) ([]model.CampaignMetric, error)
}

// Metrics sweeper tuning. All three are overridable from the environment (see the
// config package and the Helm chart) so an operator can slow the sweep down without a
// redeploy of new code.
const (
	// DefaultMetricsSweepInterval is how often the sweeper refreshes metrics. Hourly
	// sits comfortably inside every platform's reporting quota while keeping a
	// dashboard no more than an hour stale.
	DefaultMetricsSweepInterval = time.Hour

	// DefaultMetricsRestatementWindow is how far back each sweep re-reads.
	//
	// It is deliberately NOT one day. Platforms RESTATE recent days as conversions
	// mature (Google attributes a conversion to the day of the CLICK, so a conversion
	// that completes today can change a figure from several days ago). Re-reading only
	// yesterday would permanently freeze those earlier days at their provisional
	// values. The (campaign_id, metric_date) upsert makes the overlapping re-read free.
	DefaultMetricsRestatementWindow = 3 * 24 * time.Hour

	// metricsSweepCampaignLimit bounds one sweep's working set, so a large estate
	// degrades into several passes instead of one unbounded query and a long burst of
	// platform calls.
	metricsSweepCampaignLimit = 200

	// metricsSweepTimeout bounds ONE sweep pass.
	//
	// It MUST stay within CancelGracePeriod. The sweeper is cancelled first thing in
	// Shutdown and is waited on via the shared WaitGroup, so in the normal path
	// cancellation — not expiry — is what stops it, and this timeout is not an
	// additive term of the shutdown budget. This ceiling is the backstop for a pass
	// that somehow ignores cancellation: capping it at the grace period means even
	// that pass cannot outlive the container's close budget. Asserted in init().
	metricsSweepTimeout = 15 * time.Second

	// metricsFetchTimeout bounds ONE campaign's platform call within a pass, so a
	// single unresponsive platform cannot consume the entire sweep window and starve
	// every other campaign.
	metricsFetchTimeout = 5 * time.Second
)

func init() {
	// Keep the sweeper's worst case inside the shutdown budget the container
	// reserves. If someone raises metricsSweepTimeout past the grace period, a sweep
	// that ignores cancellation could still be running when the pool closes.
	if metricsSweepTimeout > CancelGracePeriod {
		panic("metricsSweepTimeout exceeds CancelGracePeriod: a sweep could outlive the container close budget")
	}
	if metricsFetchTimeout > metricsSweepTimeout {
		panic("metricsFetchTimeout exceeds metricsSweepTimeout: one campaign could consume an entire sweep")
	}
}

// MetricsSweeper periodically refreshes stored campaign metrics from the ad
// platforms.
//
// Its lifecycle mirrors the orchestrator's recovery sweeper exactly: the goroutine is
// tracked by a WaitGroup so Shutdown waits for it before the pool closes, but its
// lifetime is owned by a DEDICATED context cancelled at the very start of Shutdown.
// Cancelling up front means an in-flight sweep blocked in the DB or mid-HTTP-call is
// interrupted promptly, and the sweeper's own shutdown never draws down another
// phase's budget.
type MetricsSweeper struct {
	repo        domain.MetricsRepository
	dispatchers map[model.Provider]PlatformDispatcher

	interval    time.Duration
	restatement time.Duration
	// now is injectable so tests can pin the reporting window without sleeping.
	now func() time.Time

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// MetricsSweeperConfig carries the tunable knobs. A zero field takes its default, so
// a caller that only wants to override one does not have to restate the others.
type MetricsSweeperConfig struct {
	Interval    time.Duration
	Restatement time.Duration
}

// NewMetricsSweeper constructs a sweeper over the campaigns the given dispatchers can
// report on.
func NewMetricsSweeper(repo domain.MetricsRepository, dispatchers map[model.Provider]PlatformDispatcher, cfg MetricsSweeperConfig) *MetricsSweeper {
	if dispatchers == nil {
		dispatchers = map[model.Provider]PlatformDispatcher{}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultMetricsSweepInterval
	}
	if cfg.Restatement <= 0 {
		cfg.Restatement = DefaultMetricsRestatementWindow
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &MetricsSweeper{
		repo:        repo,
		dispatchers: dispatchers,
		interval:    cfg.Interval,
		restatement: cfg.Restatement,
		now:         time.Now,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// fetchablePlatforms returns the providers whose dispatcher implements MetricsFetcher
// AND is not merely reporting "unsupported".
//
// Only the type assertion can be checked here — whether a fetcher is a real
// implementation or a deferred stub is discovered when it is called. That is
// intentional: it keeps the deferral in one place (the dispatcher) instead of
// duplicating a roster of "really implemented" platforms that would inevitably drift.
func (s *MetricsSweeper) fetchablePlatforms() []model.Provider {
	out := make([]model.Provider, 0, len(s.dispatchers))
	for p, d := range s.dispatchers {
		if _, ok := d.(MetricsFetcher); ok {
			out = append(out, p)
		}
	}
	return out
}

// Start launches the background sweep goroutine. Call once after construction.
func (s *MetricsSweeper) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				// Bound each sweep, but derive the context from s.ctx (do NOT detach)
				// so cancelling at Shutdown interrupts a sweep already blocked
				// mid-statement rather than letting it run against a closing pool.
				sctx, cancel := context.WithTimeout(s.ctx, metricsSweepTimeout)
				n, err := s.SweepOnce(sctx)
				cancel()
				if err != nil {
					// A cancellation here is the expected outcome when Shutdown
					// interrupts an in-flight sweep, not a real failure.
					if s.ctx.Err() == nil {
						slog.ErrorContext(s.ctx, "periodic metrics sweep failed", "error", err)
					}
				} else if n > 0 {
					slog.InfoContext(s.ctx, "periodic metrics sweep refreshed campaigns", "count", n)
				}
			}
		}
	}()
}

// Shutdown stops the sweeper and waits for the in-flight sweep to unwind.
//
// Cancellation is idempotent (guarded by sync.Once) and safe whether or not Start was
// ever called. The wait is bounded by ctx so a wedged sweep cannot block shutdown past
// the caller's budget.
func (s *MetricsSweeper) Shutdown(ctx context.Context) error {
	s.once.Do(s.cancel)

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SweepOnce refreshes one batch of campaigns and returns how many were updated. It is
// exported so a test can drive a single deterministic pass without waiting on a timer.
func (s *MetricsSweeper) SweepOnce(ctx context.Context) (int, error) {
	platforms := s.fetchablePlatforms()
	if len(platforms) == 0 {
		return 0, nil
	}

	campaigns, err := s.repo.ListCampaignsForMetricsSweep(ctx, platforms, metricsSweepCampaignLimit)
	if err != nil {
		return 0, err
	}

	// The window is computed ONCE per pass so every campaign in the batch covers the
	// same days — otherwise a slow pass would drift across a midnight boundary and
	// give later campaigns a different range than earlier ones.
	to := s.now().UTC()
	from := to.Add(-s.restatement)

	updated := 0
	for _, c := range campaigns {
		// Stop promptly on cancellation rather than starting another platform call
		// that will be abandoned.
		if ctx.Err() != nil {
			return updated, ctx.Err()
		}

		d, ok := s.dispatchers[c.Platform]
		if !ok {
			continue
		}
		fetcher, ok := d.(MetricsFetcher)
		if !ok {
			continue
		}

		n, ferr := s.refreshOne(ctx, fetcher, c, from, to)
		if ferr != nil {
			// A platform that cannot report yet is an expected, permanent condition
			// for now — log it at debug so it does not drown the log every hour.
			if errors.Is(ferr, domain.ErrMetricsUnsupported) {
				slog.DebugContext(ctx, "metrics not supported for platform; skipping",
					"platform", string(c.Platform), "campaign_id", c.ID)
				continue
			}
			// Cancellation is shutdown, not a fault.
			if ctx.Err() != nil {
				return updated, ctx.Err()
			}
			// One campaign's failure must NOT abort the batch: an expired credential
			// on a single project would otherwise stop every other project's metrics
			// from ever refreshing.
			slog.WarnContext(ctx, "metrics refresh failed for campaign",
				"platform", string(c.Platform), "campaign_id", c.ID, "error", ferr)
			continue
		}
		if n > 0 {
			updated++
		}
	}
	return updated, nil
}

// refreshOne fetches and persists a single campaign's metrics.
func (s *MetricsSweeper) refreshOne(ctx context.Context, f MetricsFetcher, c *model.Campaign, from, to time.Time) (int, error) {
	// Bound the platform call so one unresponsive provider cannot consume the whole
	// sweep window. Derived from ctx so shutdown still interrupts it.
	fctx, cancel := context.WithTimeout(ctx, metricsFetchTimeout)
	defer cancel()

	rows, err := f.FetchMetrics(fctx, c, from, to)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	ptrs := make([]*model.CampaignMetric, 0, len(rows))
	for i := range rows {
		ptrs = append(ptrs, &rows[i])
	}
	// Persist on ctx, NOT fctx: the fetch budget is spent, and abandoning the write
	// would discard metrics already read from the platform.
	return s.repo.UpsertMetrics(ctx, c.ID, ptrs)
}

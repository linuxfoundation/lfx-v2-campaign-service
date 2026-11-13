// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Command reindex re-enqueues search-index messages for existing briefs and campaigns (#590):
//
//	go run ./cmd/reindex [-project <slug>] [-kind all|briefs|campaigns] [-batch 200]          # dry run
//	go run ./cmd/reindex [-project <slug>] [-kind all|briefs|campaigns] [-batch 200] -apply   # write
//
// It exists because the service has no other full-reindex path. A document indexed under an
// old contract (for example keyed on the project slug before INDEXER_RESOLVE_PROJECT_UID) is
// otherwise corrected only by its resource's next write, and a terminal write like an archive
// never comes again.
//
// It writes NOTHING but index_outbox rows, built by the service's own payload builders
// (service.BriefIndexPayload / CampaignIndexPayload), so each is byte-for-byte the message a
// write would have produced. It never publishes to NATS: the running service's relay
// delivers the rows, which is what applies the deployment's indexing settings (the service
// credential, and UID keying when INDEXER_RESOLVE_PROJECT_UID is on). Run it AFTER that flag
// is on in the target environment, or the re-sent documents are keyed on the slug again.
//
// Live briefs and campaigns get an "updated" message, archived briefs and soft-deleted
// campaigns a "deleted" one, and a bare dispatch claim is skipped (no write ever indexed it).
// Rows are read in id order, one transaction per batch, FOR SHARE, so a concurrent write is
// ordered before or after the batch and never overtaken by a stale re-send. Re-running is
// safe: it re-sends current documents.
//
// Without -apply it is a DRY RUN: every batch is read and every payload built, then rolled
// back, and only the counts are printed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	defaultBatch = 200
	maxBatch     = 1000
	// maxBatches bounds one run, so a bug that never advanced the cursor could not loop
	// forever. 100,000 batches of the smallest size is far beyond any real table.
	maxBatches = 100_000
)

type options struct {
	project string
	kind    string
	batch   int
	apply   bool
}

// store is the slice of postgres.ReindexRepo the command drives; a fake in tests.
type store interface {
	ReindexBriefs(ctx context.Context, projectID, afterID string, limit int, apply bool, build postgres.BriefPayloadFunc) (postgres.ReindexBatch, error)
	ReindexCampaigns(ctx context.Context, projectID, afterID string, limit int, apply bool, build postgres.CampaignPayloadFunc) (postgres.ReindexBatch, error)
}

func main() {
	opts, err := parseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "reindex:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn, err := config.ResolveDatabaseURL()
	if err != nil || dsn == "" {
		fmt.Fprintf(os.Stderr, "reindex: no database configured; set PGHOST/PGUSER/PGPASSWORD/PGDATABASE or %s\n", constants.EnvDatabaseURL)
		os.Exit(1)
	}
	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reindex:", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := run(ctx, opts, postgres.NewReindexRepo(pool), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "reindex:", err)
		os.Exit(1)
	}
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.project, "project", "", "only this project slug (default: every project)")
	fs.StringVar(&o.kind, "kind", "all", "what to re-index: all, briefs or campaigns")
	fs.IntVar(&o.batch, "batch", defaultBatch, fmt.Sprintf("rows per transaction (1-%d)", maxBatch))
	fs.BoolVar(&o.apply, "apply", false, "write the outbox rows (default: dry run, counts only)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	o.project = strings.TrimSpace(o.project)
	switch o.kind {
	case "all", "briefs", "campaigns":
	default:
		return o, fmt.Errorf("-kind must be all, briefs or campaigns, not %q", o.kind)
	}
	if o.batch < 1 || o.batch > maxBatch {
		return o, fmt.Errorf("-batch must be between 1 and %d", maxBatch)
	}
	return o, nil
}

type totals struct{ read, live, deleted, skipped, batches int }

func run(ctx context.Context, o options, s store, out io.Writer) error {
	mode := "DRY RUN (nothing written; pass -apply to enqueue)"
	if o.apply {
		mode = "APPLY (enqueuing outbox rows for the relay to publish)"
	}
	scope := "every project"
	if o.project != "" {
		scope = "project " + o.project
	}
	fmt.Fprintf(out, "reindex: %s, %s, batch %d\n", mode, scope, o.batch)

	if o.kind == "all" || o.kind == "briefs" {
		t, err := loop(ctx, o, func(after string) (postgres.ReindexBatch, error) {
			return s.ReindexBriefs(ctx, o.project, after, o.batch, o.apply, func(action string, b *model.CampaignBrief) ([]byte, error) {
				return service.BriefIndexPayload(action, b)
			})
		})
		report(out, "briefs", t)
		if err != nil {
			return fmt.Errorf("briefs: %w", err)
		}
	}
	if o.kind == "all" || o.kind == "campaigns" {
		t, err := loop(ctx, o, func(after string) (postgres.ReindexBatch, error) {
			return s.ReindexCampaigns(ctx, o.project, after, o.batch, o.apply, func(action string, c *model.Campaign) ([]byte, error) {
				return service.CampaignIndexPayload(action, c)
			})
		})
		report(out, "campaigns", t)
		if err != nil {
			return fmt.Errorf("campaigns: %w", err)
		}
	}
	return nil
}

func loop(ctx context.Context, o options, next func(after string) (postgres.ReindexBatch, error)) (totals, error) {
	var t totals
	after := ""
	for t.batches < maxBatches {
		if err := ctx.Err(); err != nil {
			return t, err
		}
		b, err := next(after)
		if err != nil {
			return t, err
		}
		t.batches++
		t.read += b.Read
		t.live += b.Live
		t.deleted += b.Deleted
		t.skipped += b.Skipped
		if b.Read < o.batch || b.LastID == "" {
			return t, nil
		}
		if b.LastID <= after {
			return t, fmt.Errorf("cursor did not advance past %q", after)
		}
		after = b.LastID
	}
	return t, fmt.Errorf("stopped after %d batches", maxBatches)
}

func report(out io.Writer, what string, t totals) {
	fmt.Fprintf(out, "%s: read %d, live %d, deleted %d, skipped %d (%d batches)\n",
		what, t.read, t.live, t.deleted, t.skipped, t.batches)
}

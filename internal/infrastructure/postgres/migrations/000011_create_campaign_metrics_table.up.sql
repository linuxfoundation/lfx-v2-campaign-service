-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- campaign_metrics: one row per campaign per reporting day, holding the RAW numbers
-- one ad platform reported for that day. Until this table the service was write-only:
-- it could create and pause campaigns but could not read back a single performance
-- number.
--
-- hierarchy: Project -> Brief -> Campaign -> Metrics (one row per campaign per day).
--
-- ATTRIBUTION: rows are stored per platform, exactly as the platform reported them.
-- Platforms disagree on what a conversion is and on the window it is counted in
-- (Meta defaults to 7-day click / 1-day view; Google counts at click time with
-- per-action windows and may report FRACTIONAL conversions under data-driven
-- attribution). A naive cross-platform SUM of conversions is therefore wrong in a way
-- that looks plausible. attribution_basis records each row's basis so a rollup can be
-- derived EXPLICITLY and labelled, rather than silently summing incomparable numbers.
-- See docs/campaign-metrics.md.
CREATE TABLE IF NOT EXISTS campaign_metrics (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE CASCADE: metrics are wholly derived data owned by the campaign. When
    -- a campaign row goes away the numbers describe nothing and must not be orphaned
    -- (unlike campaigns.brief_id, whose parent is never deleted while children exist).
    campaign_id  UUID        NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    -- The PLATFORM's own reporting day, in the ad account's timezone. Stored as DATE
    -- (not TIMESTAMPTZ) because that is the true grain: re-bucketing a platform's day
    -- into another timezone would invent precision the platform never reported.
    metric_date  DATE        NOT NULL,

    -- Delivery counts. Summable across platforms (they double-count a PERSON reached
    -- twice, but not an EVENT), unlike conversions below.
    impressions  BIGINT      NOT NULL DEFAULT 0 CHECK (impressions >= 0),
    clicks       BIGINT      NOT NULL DEFAULT 0 CHECK (clicks >= 0),

    -- MONEY: NUMERIC, never DOUBLE PRECISION. Float money accumulates representation
    -- error under summation; NUMERIC sums exactly. 6 decimal places holds a
    -- micro-denominated spend (Google reports cost_micros = 1/1,000,000 of the account
    -- currency) with no rounding at all.
    spend        NUMERIC(18,6) NOT NULL DEFAULT 0 CHECK (spend >= 0),

    -- NUMERIC, not an integer: Google Ads reports FRACTIONAL conversions when
    -- data-driven attribution splits credit across touchpoints. Rounding to an integer
    -- here would quietly discard that.
    conversions  NUMERIC(18,6) NOT NULL DEFAULT 0 CHECK (conversions >= 0),

    -- ISO 4217 code that `spend` is denominated in. The service does NO FX conversion
    -- (it has no rate source, and a wrong rate is worse than no rate), so a rollup over
    -- mixed currencies must omit its spend total rather than add incomparable amounts.
    currency     TEXT,

    -- The platform's conversion-counting basis for THIS row (e.g. 'google-ads:click-time').
    -- The discriminator that makes a cross-platform rollup honest instead of plausible.
    attribution_basis TEXT,

    -- The platform's own response row, verbatim, for auditability: when a number is
    -- disputed this is what the platform actually said.
    raw          JSONB,

    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Makes a re-fetch IDEMPOTENT (the repo upserts ON CONFLICT ... DO UPDATE). This
    -- is load-bearing, not defensive: platforms RESTATE recent days as conversions
    -- finalise, so the sweeper deliberately re-fetches a trailing window every pass.
    -- Without this constraint each pass would insert duplicate days and every sum
    -- would silently inflate.
    UNIQUE (campaign_id, metric_date)
);

-- No separate index for the read path. The read is
--   WHERE campaign_id = $1 AND metric_date BETWEEN $2 AND $3 ORDER BY metric_date
-- and the UNIQUE (campaign_id, metric_date) constraint's implicit index already covers
-- BOTH predicates: campaign_id is its leftmost column and metric_date is the range key.
-- A duplicate index on the same columns would cost write throughput and buy nothing.
--
-- Verified on a real DB at 73k rows / 200 campaigns: the planner uses
-- campaign_metrics_campaign_id_metric_date_key for the full predicate (a bitmap index
-- scan touching ~34 buffers, sub-millisecond), never a sequential scan. Postgres adds a
-- cheap in-memory quicksort of the ~31 matched rows for ORDER BY rather than reading
-- them in index order; that is the planner's choice on a bitmap scan and costs
-- microseconds at this row count, so it does not justify a second index.

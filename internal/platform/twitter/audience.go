// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Audience read — the X sibling of the Meta and Google audience reads: age, gender and device
// platform breakdowns across the CALLER'S campaigns, not the shared ad account.
//
// WHY ASYNCHRONOUS. X serves segmented stats only through its asynchronous stats-jobs API:
// "Segmentation is only available through asynchronous analytics queries", and the synchronous
// GET stats/accounts/:account_id takes no segmentation_type
// (https://docs.x.com/x-ads-api/analytics, Segmentation; Synchronous Analytics). So one read:
//
//  1. GET accounts/:account_id — the account's timezone (the window is computed in it) and
//     currency (X's stats carry none);
//  2. POST stats/jobs/accounts/:account_id once per segmentation (AGE, GENDER, PLATFORMS) per
//     batch of <= 20 campaign ids, with entity=CAMPAIGN, granularity=TOTAL,
//     placement=ALL_ON_TWITTER, metric_groups=ENGAGEMENT,BILLING — the account monitor's
//     parameter set plus segmentation_type;
//  3. GET stats/jobs/accounts/:account_id?job_ids=<all> until every job is SUCCESS;
//  4. downloads each job's results file (downloadStatsFile, the monitor's host allowlist and
//     caps) and folds it.
//
// Unlike the account monitor, the read does not persist a report id and return later: it waits
// inside the caller's deadline (the orchestrator's 20s metricsCallTimeout). A job not finished by
// then fails the read (503); the jobs it created finish on X's side and expire uncollected,
// costing job slots (X allows 100 concurrent per account), never data. The scope bound keeps that
// to at most len(audienceSegmentations) * MaxAudienceCampaigns/statsJobMaxEntities = 6 jobs.
//
// UNVERIFIED CONTRACT: no X Ads credentials were available when this was written. The segmented
// file shape — id_data[].segment.segment_name naming the bucket — follows X's documentation and
// developer forum, not a live account; every field is optional-and-checked and decoding fails
// closed. The dispatcher gates this read behind TWITTER_METRICS_ENABLED for that reason.
//
// ALL-NULL FILES. X's developer forum reports segmented jobs that reach SUCCESS with every metric
// null ("[Ads API v12] Metrics return NULL when using segmentation in Stats Jobs"). That cannot be
// told apart from a genuinely idle campaign set: this package already treats null as X's "no
// activity" (statsFold, and the X metrics read's firstOrZero, both read X's null for an idle
// entity as a real zero), and nothing in X's documentation says an idle entity is OMITTED rather
// than returned with null metrics. Failing closed on all-null rows would therefore 503 every read
// of an idle project forever. Instead the read succeeds and AllCountersNull flags it, so a caller
// never takes those zeros for a measurement.

// Audience dimension tokens: this broker's vocabulary, one per X segmentation_type.
const (
	AudienceDimensionAge      = "age"
	AudienceDimensionGender   = "gender"
	AudienceDimensionPlatform = "platform"
)

// audienceSegmentations are the three job families, in response order. All must succeed.
var audienceSegmentations = []struct {
	dimension    string
	segmentation string
}{
	{AudienceDimensionAge, "AGE"},
	{AudienceDimensionGender, "GENDER"},
	{AudienceDimensionPlatform, "PLATFORMS"},
}

const (
	// MaxAudienceCampaigns bounds the scope one audience read accepts: two batches of X's
	// 20-entity job cap, so at most six jobs (three segmentations) are paced, created and awaited
	// inside one request's budget. A LOCAL bound, not an X limit.
	MaxAudienceCampaigns = 2 * statsJobMaxEntities
	// defaultAudiencePollInterval is the wait between two job-status reads.
	defaultAudiencePollInterval = time.Second
	// audienceMaxPolls bounds the status reads even with no deadline on the context.
	audienceMaxPolls = 30
)

// ErrAudienceScopeTooLarge marks a read refused locally because the scope exceeds
// MaxAudienceCampaigns. Permanent while the project owns that many campaigns.
var ErrAudienceScopeTooLarge = fmt.Errorf("twitter: more than %d campaigns to scope one audience read to", MaxAudienceCampaigns)

// ErrAudienceScopeInvalid marks a read refused locally because a scope entry is not a usable X
// campaign id, or the scope is empty — an empty scope would widen the read to the whole shared
// account, and a dropped entry would present part of the project as all of it.
var ErrAudienceScopeInvalid = errors.New("twitter: audience read scope is empty or holds an invalid campaign id")

// ErrAudienceJobsUnfinished marks a read whose stats jobs had not all finished when the read's
// time (or its poll bound) ran out. Transient: a later read creates fresh jobs.
var ErrAudienceJobsUnfinished = errors.New("twitter: audience stats jobs did not finish within the read's budget")

// audienceValueRE is the charset a segment name must match to be returned verbatim: letters,
// digits, space, underscore, plus, dot and hyphen, starting with a letter or digit and ending
// with one (or '+', for "65+"-style ranges), at most 64 bytes. Nothing that could carry markup,
// control bytes or edge whitespace passes. A name outside it is not dropped or folded: the row
// is untrustworthy and the read fails. design/brief.go's TwitterAdsAudienceBucket.value carries
// the same pattern.
var audienceValueRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9 _+.\-]{0,62}[A-Za-z0-9+])?$`)

// audienceCounterRE is a JSON integer this read accepts as a counter: digits only, no sign,
// fraction or exponent. encoding/json would accept "12" (a string) into json.Number and 1e3 into
// nothing useful, so the raw literal is matched first.
var audienceCounterRE = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// currencyCodeRE is an ISO 4217 alphabetic code.
var currencyCodeRE = regexp.MustCompile(`^[A-Z]{3}$`)

// AudienceBucket is one segment's counters, summed across the scoped campaigns.
type AudienceBucket struct {
	Dimension string
	// Value is X's segment_name, verbatim (charset-checked).
	Value       string
	Impressions int64
	Clicks      int64
	// CostMicros is billed_charge_local_micro summed, in micros of AudienceInsights.Currency.
	CostMicros int64
	// Ctr is Clicks/Impressions computed AFTER summing, 0 when Impressions is 0.
	Ctr float64
}

// AudienceInsights is the audience read across the three segmentations.
type AudienceInsights struct {
	Window MetricsWindow
	// Currency is the ad account's ISO 4217 currency; "" when the account carries none.
	Currency string
	// WindowStart / WindowEnd are the [start, end) instants queried (account-local day starts),
	// and Location is the account zone they were computed in, so a caller holding this result can
	// tell (AudienceWindowBounds) whether the same window NAME still means the same instants.
	WindowStart time.Time
	WindowEnd   time.Time
	Location    *time.Location
	// AllCountersNull is true when, for at least one dimension, X returned segment rows and every
	// counter in every one of them was null or absent: either no delivery in the window, or X's
	// reported all-null segmented-stats defect. The zeros are then not a measurement.
	AllCountersNull bool
	Buckets         []AudienceBucket
}

// audienceJob is one created stats job: which dimension it reads and which campaigns it covers.
type audienceJob struct {
	id        string
	dimension string
	scope     map[string]struct{}
}

// GetAudienceInsights reads the AGE, GENDER and PLATFORMS segmentations over window (default
// WindowLast7Days) across campaignIDs on the client's account — NOT the whole account.
//
// The window NAMES are the campaign metrics read's (YESTERDAY, TODAY, LAST_7_DAYS: today;
// yesterday; today and the six days before it), but the INSTANTS differ: this read takes those
// days on the ACCOUNT's calendar ([start of the first day, start of the day after the last) in
// the account's zone), while GetCampaignMetrics (dateRangeForWindow) takes them as UTC days. On a
// non-UTC account the two reads therefore cover windows offset by the zone's UTC offset and their
// totals are not directly comparable. Longer windows are ErrUnsupportedWindow — X's
// segmented-job ceiling is 45 days; the limit keeps one window vocabulary across X's reads. A
// zone whose day starts are not whole UTC hours is ErrReportWindowNotWholeHours, as on the
// monitor: X accepts whole hours only, and a shifted window would misattribute days.
//
// All or nothing: any failed request, failed or unfinished job, untrustworthy body, foreign
// entity, duplicated row or out-of-charset segment fails the whole call; no partial buckets.
func (c *Client) GetAudienceInsights(ctx context.Context, window MetricsWindow, campaignIDs []string) (*AudienceInsights, error) {
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	switch w {
	case WindowYesterday, WindowToday, WindowLast7Days:
	default:
		return nil, ErrUnsupportedWindow
	}
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	batches, err := audienceBatches(campaignIDs)
	if err != nil {
		return nil, err
	}
	loc, currency, err := c.readAudienceAccount(ctx)
	if err != nil {
		return nil, err
	}
	start, end, err := AudienceWindowBounds(w, c.timeFn(), loc)
	if err != nil {
		return nil, err
	}
	nJobs := len(audienceSegmentations) * len(batches)
	if err := c.statsJobsFitBudget(ctx, nJobs); err != nil {
		return nil, err
	}

	jobs := make([]audienceJob, 0, nJobs)
	seenJobs := make(map[string]struct{}, nJobs)
	for _, seg := range audienceSegmentations {
		for _, batch := range batches {
			id, jerr := c.createAudienceJob(ctx, batch, start, end, seg.segmentation)
			if jerr != nil {
				return nil, fmt.Errorf("get x audience insights (%s): %w", seg.dimension, jerr)
			}
			if _, dup := seenJobs[id]; dup {
				return nil, errors.New("get x audience insights: x returned the same stats job id for two jobs")
			}
			seenJobs[id] = struct{}{}
			scope := make(map[string]struct{}, len(batch))
			for _, cid := range batch {
				scope[cid] = struct{}{}
			}
			jobs = append(jobs, audienceJob{id: id, dimension: seg.dimension, scope: scope})
		}
	}

	urls, err := c.awaitAudienceJobs(ctx, jobs)
	if err != nil {
		return nil, fmt.Errorf("get x audience insights: %w", err)
	}

	fold := newAudienceFold()
	for _, job := range jobs {
		data, derr := c.downloadStatsFile(ctx, urls[job.id])
		if derr != nil {
			return nil, fmt.Errorf("get x audience insights (%s): %w", job.dimension, derr)
		}
		if ferr := fold.add(job, data); ferr != nil {
			return nil, fmt.Errorf("get x audience insights (%s): %w", job.dimension, ferr)
		}
	}
	return &AudienceInsights{
		Window: w, Currency: currency, WindowStart: start, WindowEnd: end, Location: loc,
		AllCountersNull: fold.allNull(), Buckets: fold.finish(),
	}, nil
}

// audienceBatches validates and deduplicates the scope and splits it into job-sized batches.
// An empty, invalid or oversized scope is refused before any request.
func audienceBatches(campaignIDs []string) ([][]string, error) {
	if len(campaignIDs) == 0 {
		return nil, ErrAudienceScopeInvalid
	}
	seen := make(map[string]struct{}, len(campaignIDs))
	ids := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		if err := ValidateCampaignID(id); err != nil {
			return nil, fmt.Errorf("%w (%d bytes)", ErrAudienceScopeInvalid, len(id))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > MaxAudienceCampaigns {
		return nil, fmt.Errorf("%w: %d campaigns", ErrAudienceScopeTooLarge, len(ids))
	}
	batches := make([][]string, 0, (len(ids)+statsJobMaxEntities-1)/statsJobMaxEntities)
	for i := 0; i < len(ids); i += statsJobMaxEntities {
		batches = append(batches, ids[i:min(i+statsJobMaxEntities, len(ids))])
	}
	return batches, nil
}

// AudienceWindowBounds is window's [start, end) on the account's calendar: TODAY is today, YESTERDAY
// is yesterday, LAST_7_DAYS is today and the six days before it — dateRangeForWindow's days —
// each day starting at localDayStart. Refused (ErrReportWindowNotWholeHours) when either bound is
// not a whole UTC hour.
func AudienceWindowBounds(w MetricsWindow, now time.Time, loc *time.Location) (start, end time.Time, err error) {
	var first, afterLast int
	switch w {
	case WindowToday:
		first, afterLast = 0, 1
	case WindowYesterday:
		first, afterLast = -1, 0
	case WindowLast7Days:
		first, afterLast = -6, 1
	default:
		return time.Time{}, time.Time{}, ErrUnsupportedWindow
	}
	y, m, d := now.In(loc).Date()
	start = localDayStart(y, m, d+first, loc).UTC()
	end = localDayStart(y, m, d+afterLast, loc).UTC()
	if !start.Equal(start.Truncate(time.Hour)) || !end.Equal(end.Truncate(time.Hour)) {
		return time.Time{}, time.Time{}, ErrReportWindowNotWholeHours
	}
	return start, end, nil
}

// audienceAccountWire is the part of GET accounts/:account_id the audience read uses. Pointers,
// so an absent field is told from an empty one.
type audienceAccountWire struct {
	ID       *string `json:"id"`
	Timezone *string `json:"timezone"`
	Currency *string `json:"currency"`
}

// readAudienceAccount reads the account's timezone and currency in ONE uncached GET, held to the
// trust rules of every other audience response: identityjson over the raw body, and the body must
// describe the configured account. The timezone is required (guessing UTC would shift every day);
// the currency is optional — absent or null leaves it "" — but a present one must be ISO 4217.
// Upstream text is never echoed.
func (c *Client) readAudienceAccount(ctx context.Context) (*time.Location, string, error) {
	resp, err := c.request(ctx, http.MethodGet, "")
	if err != nil {
		return nil, "", fmt.Errorf("read x ads account: %w", err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, "", errors.New("read x ads account: response carried no account object")
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, "", fmt.Errorf("read x ads account: %w", err)
	}
	var acct audienceAccountWire
	if err := json.Unmarshal(resp.Data, &acct); err != nil {
		return nil, "", errors.New("read x ads account: response is not an account object")
	}
	if acct.ID == nil || *acct.ID != c.account.AccountID {
		return nil, "", errors.New("read x ads account: response does not describe the connection's account")
	}
	if acct.Timezone == nil || strings.TrimSpace(*acct.Timezone) == "" {
		return nil, "", errors.New("read x ads account: account has no timezone")
	}
	loc, lerr := time.LoadLocation(strings.TrimSpace(*acct.Timezone))
	if lerr != nil {
		return nil, "", errors.New("read x ads account: account timezone is not a recognised IANA zone")
	}
	currency := ""
	if acct.Currency != nil {
		if !currencyCodeRE.MatchString(*acct.Currency) {
			return nil, "", fmt.Errorf("read x ads account: currency is not an ISO 4217 code (%d bytes)", len(*acct.Currency))
		}
		currency = *acct.Currency
	}
	return loc, currency, nil
}

// createAudienceJob creates one segmented stats job and returns its id_str, refusing a response
// identityjson would not trust.
func (c *Client) createAudienceJob(ctx context.Context, campaignIDs []string, start, end time.Time, segmentation string) (string, error) {
	resp, err := c.postStatsJob(ctx, campaignIDs, start, end, segmentation)
	if err != nil {
		return "", err
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return "", errors.New("create x stats job: response carried no job object")
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return "", fmt.Errorf("create x stats job: %w", err)
	}
	var job statsJobElement
	if json.Unmarshal(resp.Data, &job) != nil {
		return "", errors.New("create x stats job: response carried no job object")
	}
	if !validStatsJobID(job.IDStr) {
		return "", errors.New("create x stats job: response carried no usable id_str")
	}
	return job.IDStr, nil
}

// awaitAudienceJobs reads every job's status with ONE request per poll (job_ids, "up to 200",
// https://docs.x.com/x-ads-api/analytics) until all are SUCCESS, and returns each job's file
// url. The first read is immediate; later ones wait audiencePollInterval. A FAILED/CANCELLED job,
// a SUCCESS job with no url, an unrecognised status, an answer naming a job not asked about or
// naming one twice, or an untrustworthy body fails the read. A job absent from the answer is
// still pending (X's forum reports that for a job_ids list), as on the monitor.
func (c *Client) awaitAudienceJobs(ctx context.Context, jobs []audienceJob) (map[string]string, error) {
	ids := make([]string, 0, len(jobs))
	asked := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.id)
		asked[j.id] = struct{}{}
	}
	for poll := 0; ; poll++ {
		if poll > 0 {
			if poll >= audienceMaxPolls {
				return nil, fmt.Errorf("%w (%d status reads)", ErrAudienceJobsUnfinished, poll)
			}
			if err := sleepCtx(ctx, c.audiencePollInterval); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrAudienceJobsUnfinished, err)
			}
		}
		urls, done, err := c.readAudienceJobs(ctx, ids, asked)
		if err != nil {
			return nil, err
		}
		if done {
			return urls, nil
		}
	}
}

// readAudienceJobs is one status read of awaitAudienceJobs.
func (c *Client) readAudienceJobs(ctx context.Context, ids []string, asked map[string]struct{}) (map[string]string, bool, error) {
	resp, err := c.doRequestAbs(ctx, http.MethodGet, c.statsJobsURL(), "stats/jobs",
		map[string]string{"job_ids": strings.Join(ids, ",")}, true /* idempotent: read */)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, fmt.Errorf("%w: read x stats jobs: %w", ErrAudienceJobsUnfinished, err)
		}
		return nil, false, fmt.Errorf("read x stats jobs: %w", err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, false, errors.New("read x stats jobs: 2xx response carried no data field")
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, false, fmt.Errorf("read x stats jobs: %w", err)
	}
	var els []statsJobElement
	if err := json.Unmarshal(resp.Data, &els); err != nil {
		return nil, false, errors.New("read x stats jobs: data is not a list of jobs")
	}
	byID := make(map[string]statsJobElement, len(els))
	for _, el := range els {
		if _, ok := asked[el.IDStr]; !ok {
			return nil, false, errors.New("read x stats jobs: the answer names a job this read did not ask about")
		}
		if _, dup := byID[el.IDStr]; dup {
			return nil, false, errors.New("read x stats jobs: the answer names one job twice")
		}
		byID[el.IDStr] = el
	}
	pending := false
	urls := make(map[string]string, len(ids))
	for _, id := range ids {
		el, ok := byID[id]
		if !ok {
			pending = true
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(el.Status)) {
		case jobStatusQueued, jobStatusProcessing:
			pending = true
		case jobStatusFailed, jobStatusFailure, jobStatusCancelled:
			return nil, false, errors.New("read x stats jobs: x failed or cancelled an audience stats job")
		case jobStatusSuccess:
			if el.URL == nil || strings.TrimSpace(*el.URL) == "" {
				return nil, false, errors.New("read x stats jobs: a finished job offers no results file")
			}
			urls[id] = strings.TrimSpace(*el.URL)
		default:
			return nil, false, errors.New("read x stats jobs: a job has an unrecognised status")
		}
	}
	return urls, !pending, nil
}

// audienceFileWire is a segmented results file. Data is a pointer so an absent or null `data`
// (a body proving nothing) is told from X's authoritative empty `[]`.
type audienceFileWire struct {
	Data *[]audienceEntityWire `json:"data"`
}

type audienceEntityWire struct {
	ID     *string               `json:"id"`
	IDData *[]audienceIDDataWire `json:"id_data"`
}

type audienceIDDataWire struct {
	Segment *audienceSegmentWire `json:"segment"`
	Metrics *audienceMetricsWire `json:"metrics"`
}

// audienceSegmentWire carries segment_name, the bucket's value. segment_value is not read: X's
// developer forum documents it as meaningless for AGE, and the name is what X displays.
type audienceSegmentWire struct {
	SegmentName *string `json:"segment_name"`
}

// audienceMetricsWire keeps each counter RAW: absent and null are X's "no activity" (0), anything
// else must be a one-element array (granularity=TOTAL) of a non-negative integer or null.
type audienceMetricsWire struct {
	Impressions            json.RawMessage `json:"impressions"`
	Clicks                 json.RawMessage `json:"clicks"`
	BilledChargeLocalMicro json.RawMessage `json:"billed_charge_local_micro"`
}

// audienceFold sums segments per (dimension, value) across every job's file.
type audienceFold struct {
	totals map[string]*AudienceBucket
	order  []string
	// rows is every (dimension, campaign, segment) seen: a repeat would double-count.
	rows map[string]struct{}
	// entities is every (dimension, campaign) seen: a campaign appears once per segmentation.
	entities map[string]struct{}
	// dimRows counts segment rows per dimension; dimMeasured records whether any counter in
	// that dimension carried a value (a literal 0 included). See allNull.
	dimRows     map[string]int
	dimMeasured map[string]bool
}

func newAudienceFold() *audienceFold {
	return &audienceFold{
		totals: map[string]*AudienceBucket{}, rows: map[string]struct{}{}, entities: map[string]struct{}{},
		dimRows: map[string]int{}, dimMeasured: map[string]bool{},
	}
}

// allNull reports whether any dimension returned rows whose every counter was null or absent.
// Per dimension, not per job: a second batch of idle campaigns beside a measured first batch is
// ordinary, while a whole segmentation with no measurement is the defect's signature.
func (f *audienceFold) allNull() bool {
	for dim, n := range f.dimRows {
		if n > 0 && !f.dimMeasured[dim] {
			return true
		}
	}
	return false
}

// add folds one job's decompressed file. Every message is this package's own text; no byte of
// the file is echoed.
func (f *audienceFold) add(job audienceJob, data []byte) error {
	if err := identityjson.Check(data); err != nil {
		return fmt.Errorf("decode x stats file: %w", err)
	}
	var file audienceFileWire
	if err := json.Unmarshal(data, &file); err != nil {
		return errors.New("decode x stats file: not a segmented stats document")
	}
	if file.Data == nil {
		return errors.New("decode x stats file: no data field")
	}
	for i, ent := range *file.Data {
		if ent.ID == nil {
			return fmt.Errorf("decode x stats file: entity %d has no id", i)
		}
		if _, ok := job.scope[*ent.ID]; !ok {
			return fmt.Errorf("decode x stats file: entity %d names a campaign outside this job's scope (%d bytes)", i, len(*ent.ID))
		}
		entKey := job.dimension + "\x00" + *ent.ID
		if _, dup := f.entities[entKey]; dup {
			return fmt.Errorf("decode x stats file: entity %d repeats a campaign", i)
		}
		f.entities[entKey] = struct{}{}
		if ent.IDData == nil {
			return fmt.Errorf("decode x stats file: entity %d has no id_data", i)
		}
		for k, seg := range *ent.IDData {
			if err := f.addSegment(job.dimension, *ent.ID, seg); err != nil {
				return fmt.Errorf("decode x stats file: entity %d segment %d: %w", i, k, err)
			}
		}
	}
	return nil
}

func (f *audienceFold) addSegment(dimension, campaignID string, seg audienceIDDataWire) error {
	if seg.Segment == nil || seg.Segment.SegmentName == nil {
		return errors.New("segment name is missing")
	}
	name := *seg.Segment.SegmentName
	if !audienceValueRE.MatchString(name) {
		return fmt.Errorf("segment name is outside the accepted charset (%d bytes)", len(name))
	}
	if seg.Metrics == nil {
		return errors.New("metrics are missing")
	}
	rowKey := dimension + "\x00" + campaignID + "\x00" + name
	if _, dup := f.rows[rowKey]; dup {
		return errors.New("duplicate row for one campaign and segment")
	}
	f.rows[rowKey] = struct{}{}
	impressions, mI, err := audienceCounter(seg.Metrics.Impressions)
	if err != nil {
		return fmt.Errorf("impressions %w", err)
	}
	clicks, mC, err := audienceCounter(seg.Metrics.Clicks)
	if err != nil {
		return fmt.Errorf("clicks %w", err)
	}
	cost, mS, err := audienceCounter(seg.Metrics.BilledChargeLocalMicro)
	if err != nil {
		return fmt.Errorf("billed_charge_local_micro %w", err)
	}
	f.dimRows[dimension]++
	if mI || mC || mS {
		f.dimMeasured[dimension] = true
	}
	key := dimension + "\x00" + name
	b, ok := f.totals[key]
	if !ok {
		b = &AudienceBucket{Dimension: dimension, Value: name}
		f.totals[key] = b
		f.order = append(f.order, key)
	}
	if b.Impressions > math.MaxInt64-impressions || b.Clicks > math.MaxInt64-clicks || b.CostMicros > math.MaxInt64-cost {
		return errors.New("counters overflow int64 when summed across campaigns")
	}
	b.Impressions += impressions
	b.Clicks += clicks
	b.CostMicros += cost
	return nil
}

// audienceCounter reads one TOTAL-granularity metric and reports whether it carried a value.
// Absent or null (the whole metric, or its one bucket) is X's "no activity" and reads 0 with
// measured=false, exactly as the monitor's fold treats it; an array of any other length, a
// non-integer, a negative or an int64 overflow is an error.
func audienceCounter(raw json.RawMessage) (n int64, measured bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	var buckets []json.RawMessage
	if err := json.Unmarshal(raw, &buckets); err != nil {
		return 0, false, errors.New("is not an array")
	}
	if len(buckets) != 1 {
		return 0, false, fmt.Errorf("has %d buckets, want exactly 1 for granularity TOTAL", len(buckets))
	}
	v := strings.TrimSpace(string(buckets[0]))
	if v == "null" {
		return 0, false, nil
	}
	if !audienceCounterRE.MatchString(v) {
		return 0, false, fmt.Errorf("is not a non-negative integer (%d bytes)", len(v))
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("overflows int64 (%d bytes)", len(v))
	}
	return n, true, nil
}

// audienceDimensionRank orders the response's dimensions.
var audienceDimensionRank = map[string]int{AudienceDimensionAge: 0, AudienceDimensionGender: 1, AudienceDimensionPlatform: 2}

// finish computes CTR after summing (averaging per-campaign CTRs would weight a ten-impression
// campaign like a thousand-impression one) and orders by dimension, impressions descending, then
// value, so the response is deterministic for a given upstream answer. Non-nil when empty.
func (f *audienceFold) finish() []AudienceBucket {
	out := make([]AudienceBucket, 0, len(f.order))
	for _, k := range f.order {
		b := *f.totals[k]
		if b.Impressions > 0 {
			b.Ctr = float64(b.Clicks) / float64(b.Impressions)
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := audienceDimensionRank[out[i].Dimension], audienceDimensionRank[out[j].Dimension]
		if ri != rj {
			return ri < rj
		}
		if out[i].Impressions != out[j].Impressions {
			return out[i].Impressions > out[j].Impressions
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// withAudiencePollInterval replaces the wait between job-status reads. Unexported TEST seam.
func withAudiencePollInterval(d time.Duration) Option {
	return func(c *Client) { c.audiencePollInterval = d }
}

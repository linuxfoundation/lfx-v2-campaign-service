// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ---------------------------------------------------------------------------
// Marketing-email statistics (LFXV2-3058)
//
// GET /marketing/v3/emails/statistics/list?startTimestamp&endTimestamp&emailIds
// is a pure read, so it passes idempotent=true and a 429 is retried.
//
// The reasoning behind the two fail-closed guards below — why an unrecognized
// counter vocabulary is an error rather than a zero, and why the returned email
// list is checked against the id we asked for — is in
// docs/knowledge/code/internal-platform-hubspot.md.
// ---------------------------------------------------------------------------

const statisticsPath = "/marketing/v3/emails/statistics/list"

var (
	// ErrUnsupportedWindow reports a model.MetricsWindow this client cannot express as a
	// timestamp range. HubSpot takes arbitrary ISO-8601 instants, so today every window is
	// supported and this is unreachable from the switch — it exists so that adding a window
	// to the shared vocabulary without mapping it here fails loudly instead of silently
	// querying the zero time.
	ErrUnsupportedWindow = errors.New("hubspot: unsupported metrics window")

	// ErrStatisticsFilterNotHonored reports that the response's `emails` list is not
	// exactly the one id we filtered on — either it omits that id or it names others
	// alongside it. Both mean the filter was not applied as issued, so the aggregate
	// describes a set we did not ask for and none of the response is trustworthy;
	// reporting its numbers as this campaign's would attribute strangers' sends to it.
	ErrStatisticsFilterNotHonored = errors.New("hubspot: statistics response does not cover exactly the requested email")

	// ErrEmailNotSentInWindow reports an empty `emails` list: HubSpot did not include this
	// email in the requested span. The documented reason is that the span selects emails by
	// SEND time, so a window that does not contain the send date returns nothing.
	//
	// This is an error rather than a zeroed result because zeros here are indistinguishable
	// from the other zero — an email that WAS sent in the window and simply earned no opens.
	// Collapsing the two answers "this campaign got no engagement" about a campaign that may
	// be getting plenty, which is the worse of the two lies a metrics read can tell.
	ErrEmailNotSentInWindow = errors.New("hubspot: the requested window does not contain this email's send date")

	// ErrUnrecognizedCounters reports a `counters` map carrying not one key from HubSpot's
	// counter vocabulary — whether because the keys were renamed or because the field was
	// absent entirely. Since `counters` is an OPEN map in the v3 schema, either shape
	// decodes cleanly to zeros, so an email that really sent would report as having sent
	// nothing: a dead campaign rather than a broken integration.
	ErrUnrecognizedCounters = errors.New("hubspot: statistics response carried no recognized counter")

	// ErrRenamedCounter reports the PARTIAL rename ErrUnrecognizedCounters cannot see: a
	// map that still carries recognized keys, so the guard above passes, while one of the
	// six keys this client MAPS has gone missing and an unrecognized key has appeared in
	// the same response. `{"sent":1000,"emailsOpened":400}` is the shape — `sent` keeps the
	// vocabulary guard happy and the `open` lookup returns an authoritative 0.
	//
	// The two conditions are required TOGETHER, and that conjunction is the whole design.
	// A missing mapped key on its own is not evidence of anything: HubSpot may simply omit
	// a counter that is zero, and the client cannot verify either way from an auth-gated
	// spec — erroring on absence alone would reject ordinary quiet emails. An unknown key
	// on its own is not evidence either: adding a counter is the likeliest way this
	// vocabulary evolves, and rejecting additive change would break the client on an
	// upstream release that took nothing away. Only the two together carry the signature
	// of a rename, because a renamed key does not vanish — it reappears under a new name.
	//
	// The two CAN co-occur innocently: an upstream release adds a counter in the same week
	// an email omits a zero-valued one. That case errors, deliberately. At the moment both
	// signals are present this client cannot tell an addition-plus-omission from a rename,
	// and the rule this whole file is built on is that an answer it cannot VERIFY is an
	// error rather than a clean zero — a false "we cannot read this" is recoverable by
	// widening the vocabulary, while the false zero it would otherwise return is a live
	// campaign reported as dead. TestGetEmailMetrics_PartiallyRenamedCounterVocabularyIsAnError
	// pins the combination so the trade is visible rather than incidental. Note the quiet
	// `notsent`/`pending` path is NOT affected: those are known keys, so no unknown key is
	// present and the guard cannot fire however many mapped keys are absent.
	ErrRenamedCounter = errors.New("hubspot: a mapped counter is absent while an unrecognized counter is present")

	// ErrNegativeCounter reports a counter below zero. These are event counts, so a
	// negative is malformed upstream data, and passing it through yields negative
	// impressions and a nonsensical CTR that reads as authoritative to every consumer.
	// The LinkedIn, Meta and Reddit readers reject negatives for the same reason.
	ErrNegativeCounter = errors.New("hubspot: statistics response carried a negative counter")
)

// The counters this client maps onto its result. HubSpot's v3 schema types `counters` as
// map[string]int with no enumerated keys, so these names come from the counter vocabulary
// HubSpot's email APIs have used since v1. They are named constants (not literals at the
// call site) so TestCounterKeysAreTheDocumentedVocabulary can pin the exact strings: a
// rename upstream should break a test here, not quietly zero a dashboard.
const (
	counterSent         = "sent"
	counterDelivered    = "delivered"
	counterOpen         = "open"
	counterClick        = "click"
	counterBounce       = "bounce"
	counterUnsubscribed = "unsubscribed"
)

// knownCounterVocabulary is the PROBE set for the ErrUnrecognizedCounters guard, and is
// deliberately WIDER than the six keys above. A window in which an email was created but
// never sent can legitimately come back carrying only counters this client does not map
// (`notsent`, `pending`), and treating that as a vocabulary change would turn an ordinary
// empty result into an error. The guard's job is to distinguish "HubSpot's vocabulary is
// intact and these numbers are zero" from "HubSpot's vocabulary changed and we are reading
// nothing at all", so it must recognize the whole vocabulary while mapping only part of it.
//
// `contactslost`, `hardbounced` and `softbounced` appear in HubSpot's v3 response examples
// and were missing from the first version of this set. Omitting a real key is not a
// harmless gap: an unrecognized key is half of the ErrRenamedCounter conjunction, so a
// perfectly ordinary response carrying `hardbounced` alongside an omitted zero-valued
// mapped counter was rejected as a rename. Widening the PROBE set can only ever reduce
// false errors — it never widens what this client READS, which is `mappedCounters` below.
var knownCounterVocabulary = map[string]struct{}{
	counterSent: {}, counterDelivered: {}, counterOpen: {}, counterClick: {},
	counterBounce: {}, counterUnsubscribed: {},
	"contactslost": {}, "deferred": {}, "dropped": {}, "hardbounced": {},
	"notsent": {}, "pending": {}, "reply": {}, "selected": {},
	"softbounced": {}, "spamreport": {}, "suppressed": {},
}

// statisticsData mirrors HubSpot's EmailStatisticsData. Only `counters` is decoded:
// `ratios` is discarded because Ctr is COMPUTED here from clicks and opens, keeping one
// definition of the ratio across every platform rather than adopting each platform's own;
// `deviceBreakdown` and `qualifierStats` have no consumer.
type statisticsData struct {
	Counters map[string]int64 `json:"counters"`
}

// aggregateStatistics mirrors HubSpot's AggregateEmailStatistics.
//
// `campaignAggregations` is NOT decoded. It is keyed by email-campaign id, not by email
// id, so indexing it with the id we filtered on would silently miss and fall through to a
// zero value. Because the request filters to exactly one email, `aggregate` already IS
// that email's aggregate, and `emails` is what proves the filter was applied.
//
// `Emails` is a POINTER so that an absent or null field stays distinguishable from `[]`.
// A value slice makes both decode to nil, and the two mean opposite things: an explicit
// empty list is HubSpot answering that the span held no send, while a MISSING field is
// the disappearance of the only evidence that the filter was honoured at all. Reporting
// the second as the first would tell a caller "wrong window" about a schema break.
type aggregateStatistics struct {
	Emails    *[]int64       `json:"emails"`
	Aggregate statisticsData `json:"aggregate"`
}

// supportedMetricsWindows is exactly the set timeRangeForWindow maps to a timestamp range.
//
// It enumerates HubSpot's OWN set rather than deferring to model.IsValidMetricsWindow, and
// that distinction is the point: the shared vocabulary is where a new window is ADDED, and
// a validator that accepts everything the model knows would accept a window this client
// has not mapped. The dispatcher's whole reason for calling the validator first is to fail
// a permanent 400 before resolving credentials; deferring to the model would let the new
// window pass validation, resolve credentials, and only then fail — reporting a permanent
// defect through whatever status the connection state produced. Enumerating here makes an
// unmapped addition fail CLOSED at the earliest point, and
// TestValidateMetricsWindowMatchesTimeRangeForWindow keeps the two in step. Same shape as
// internal/platform/linkedin/metrics.go.
var supportedMetricsWindows = map[model.MetricsWindow]struct{}{
	model.MetricsWindowToday:      {},
	model.MetricsWindowYesterday:  {},
	model.MetricsWindowLast7Days:  {},
	model.MetricsWindowLast14Days: {},
	model.MetricsWindowLast30Days: {},
	model.MetricsWindowThisMonth:  {},
	model.MetricsWindowLastMonth:  {},
}

// ValidateMetricsWindow reports whether this client can express window as a timestamp
// range. Callers use it to fail an unsupported window BEFORE resolving credentials, so a
// permanent 400 is not reported as a retryable connection error.
func ValidateMetricsWindow(window model.MetricsWindow) error {
	if _, ok := supportedMetricsWindows[window]; !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}
	return nil
}

// GetEmailMetrics reads live statistics for one marketing email over one window.
//
// # What the window actually does, and what it does not
//
// HubSpot's span is NOT an event-time filter, and this is the single most important thing
// to know before reading a number out of the result. The generated contract describes the
// operation as returning "aggregated statistics of emails SENT in a specified time span",
// and the response's `emails` field as the list of emails sent during it. So the span
// selects WHICH EMAILS are in scope, by send date; the counters that come back are that
// email's totals to date, not the opens and clicks that occurred inside the window.
//
// Two consequences follow, and callers must not paper over either:
//
//   - A window containing the send date returns the email's aggregate counters. Asking for
//     `today` and `last_30_days` on an email sent this morning returns the SAME numbers.
//     The window does not narrow them.
//   - A window not containing the send date returns nothing at all, which this function
//     reports as ErrEmailNotSentInWindow rather than as zeros. See that sentinel for why
//     zeros would be the wrong answer.
//
// model.CampaignMetrics.Window therefore records what was ASKED, not a period the counters
// are scoped to. Presenting these as "opens in the last 7 days" would be false. Getting
// genuine event-time windowing needs a different HubSpot source (the email-events API,
// which timestamps each open and click) and is deliberately not attempted here.
//
// emailID is the HubSpot marketing-email id stored as the campaign's PlatformCampaignID.
// It is validated as a canonical positive decimal integer before use: it is interpolated
// into a query the API filters on, and HubSpot types emailIds as an integer list, so a
// value that is not one would either be rejected upstream or — worse — ignored, widening
// the filter to the whole portal.
func (c *Client) GetEmailMetrics(ctx context.Context, emailID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	id, err := parseEmailID(emailID)
	if err != nil {
		return nil, err
	}
	start, end, err := c.timeRangeForWindow(window)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	// RFC3339 in UTC. The API documents these as ISO-8601 and the two agree for this shape.
	//
	// Nano, not plain RFC3339, because plain RFC3339 TRUNCATES the fraction: `end` is the
	// final millisecond of the day and would go out as 23:59:59Z, silently dropping 999ms
	// of it. Under an inclusive bound that is a real hole at the end of every window, and
	// it contradicts the last-millisecond contract timeRangeForWindow documents. Nano emits
	// only the digits present, so `start` (exactly midnight) is unchanged.
	q.Set("startTimestamp", start.Format(time.RFC3339Nano))
	q.Set("endTimestamp", end.Format(time.RFC3339Nano))
	q.Set("emailIds", strconv.FormatInt(id, 10))

	raw, err := c.doRequest(ctx, http.MethodGet, statisticsPath+"?"+q.Encode(), nil, true)
	if err != nil {
		return nil, err
	}

	var resp aggregateStatistics
	if err := json.Unmarshal(raw, &resp); err != nil {
		// The CAUSE is discarded, not just the body. Not quoting the body is not enough:
		// json.SyntaxError and json.UnmarshalTypeError carry fragments of the input in
		// their own messages — an overflowing numeric literal is reproduced verbatim — and
		// BriefService.GetCampaignMetrics's default branch logs safeErrSummary(err), which
		// truncates but does not redact. So the diagnostic is built from constants plus a
		// length, which is what actually distinguishes "empty response" from "10 MiB of
		// something". Matches internal/platform/linkedin/metrics.go. Nothing is lost to
		// callers: encoding/json's errors are concrete types nobody here matches on.
		return nil, fmt.Errorf("hubspot: decode email statistics response: malformed JSON (%d bytes)", len(raw))
	}

	// An ABSENT `emails` field is a schema break, not an answer. It is the only evidence
	// the filter was honoured, and without it the aggregate below describes an unknown set
	// of emails. It must NOT reach the empty-list branch: "the field HubSpot uses to prove
	// the filter is gone" is not the same claim as "the span held no send", and the second
	// invites a caller to retry with a different window against a response shape that will
	// never carry what it needs.
	if resp.Emails == nil {
		return nil, fmt.Errorf("%w: response omitted the emails field entirely",
			ErrStatisticsFilterNotHonored)
	}
	emails := *resp.Emails

	// An EMPTY `emails` means HubSpot did not include this email in the span at all — per
	// the contract, because the span selects by SEND time and does not contain its send
	// date. It does NOT mean the email earned no engagement, and the two must not collapse:
	// an email that WAS sent in the window with no opens comes back present, with a `sent`
	// counter. Returning zeros here would make "you picked the wrong window" and "nobody
	// opened it" the same answer.
	if len(emails) == 0 {
		return nil, ErrEmailNotSentInWindow
	}

	// A NON-empty list must name our id and NOTHING ELSE. Presence alone is not enough:
	// the request supplies exactly one `emailIds` value and `aggregate` is the aggregation
	// over the emails the response covers, so a list of [1, 4242, 9999] proves the filter
	// widened — its counters include two strangers' sends, and reporting them as this
	// campaign's is precisely the misattribution this guard exists to prevent. Either the
	// filter was honoured, in which case the list is exactly what we asked for, or it was
	// not, in which case none of the response is trustworthy. There is no middle reading.
	if !isExactlyID(emails, id) {
		return nil, fmt.Errorf("%w: asked for %d, response covers %d email(s)",
			ErrStatisticsFilterNotHonored, id, len(emails))
	}

	// The vocabulary guard, and the reason it is not `len(counters) > 0`: a MISSING or
	// renamed `counters` field decodes to a nil map, which that form waves through — every
	// lookup returns 0 and an email HubSpot has just told us it covers reports as having
	// sent nothing. The absent map is the same schema break as a renamed key set, and it is
	// the one the narrower check could not see.
	//
	// The guard is unconditional now that an empty `emails` list returns above. It used to
	// carry a `|| len(resp.Emails) > 0` term to let zeros through for that case, on the
	// mistaken reading that an empty list meant "no activity". There is no longer any path
	// here on which an all-zero counter map is a legitimate answer.
	counters := resp.Aggregate.Counters
	if !hasKnownCounter(counters) {
		return nil, ErrUnrecognizedCounters
	}
	// The guard above answers "is the vocabulary recognizable AT ALL"; this one answers
	// "is the part of it we READ still here". A rename that leaves any one known key
	// standing satisfies the first and is invisible to it.
	// `missing` comes from the static mappedCounters list, so naming it is safe and it is
	// the half of the diagnosis that tells an operator what to go look at. The unrecognized
	// key does NOT get named: it is arbitrary text from an open response map, and it would
	// travel into the service's logged error summary. Its COUNT carries the part of the
	// signal that matters — that the conjunction fired — without the payload.
	if missing, unknownCount, ok := renamedCounter(counters); ok {
		return nil, fmt.Errorf("%w: %q is absent while %d unrecognized counter(s) are present",
			ErrRenamedCounter, missing, unknownCount)
	}
	// Counts, so a negative is malformed upstream data rather than a small number. Checked
	// across the WHOLE map, not just the six mapped keys: a negative anywhere in the
	// counter set is evidence the payload is wrong, and the five keys read below would be
	// no more trustworthy for having stayed positive.
	// Neither the key nor the value is interpolated when the key is not one this client
	// knows: both are arbitrary response content, and this error reaches a log line. A key
	// from the static vocabulary is safe to name and is the useful case, so that one IS
	// named; anything else is reported by shape alone.
	for _, k := range sortedKeys(counters) {
		if counters[k] < 0 {
			if _, known := knownCounterVocabulary[k]; known {
				return nil, fmt.Errorf("%w: %q is negative", ErrNegativeCounter, k)
			}
			return nil, fmt.Errorf("%w: an unrecognized counter is negative", ErrNegativeCounter)
		}
	}

	opens := counters[counterOpen]
	clicks := counters[counterClick]
	return &model.CampaignMetrics{
		CampaignID:  emailID,
		Window:      window,
		Impressions: opens,
		Clicks:      clicks,
		// Zero, always. HubSpot charges nothing per send, so there is no platform-reported
		// cost to read — see the concept doc on why this must not be blended into a
		// cross-channel cost-per-acquisition.
		CostMicros: 0,
		Ctr:        ratio(clicks, opens),
		Email: &model.EmailMetrics{
			Sent:         counters[counterSent],
			Delivered:    counters[counterDelivered],
			Opens:        opens,
			Clicks:       clicks,
			Bounces:      counters[counterBounce],
			Unsubscribes: counters[counterUnsubscribed],
		},
	}, nil
}

// parseEmailID accepts only a canonical positive decimal integer. The round-trip compare
// rejects the non-canonical forms ParseInt accepts — `+123`, `0123`, and (with surrounding
// whitespace) values that would need trimming — so the string we interpolate is exactly
// the number we validated. Whitespace is not trimmed away: a padded id means the stored
// value is malformed, and answering "no metrics" for it would hide that.
func parseEmailID(emailID string) (int64, error) {
	id, err := strconv.ParseInt(emailID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != emailID {
		return 0, fmt.Errorf("hubspot: malformed marketing-email id")
	}
	return id, nil
}

// isExactlyID reports whether ids names want and nothing else. An extra id is a widened
// filter, not extra information: the aggregate then covers emails we did not ask about.
func isExactlyID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id != want {
			return false
		}
	}
	return len(ids) > 0
}

// hasKnownCounter reports whether at least one key belongs to HubSpot's counter
// vocabulary. A nil or empty map has none, which is what makes an absent `counters` field
// indistinguishable from a renamed one at the call site — deliberately.
func hasKnownCounter(counters map[string]int64) bool {
	for k := range counters {
		if _, ok := knownCounterVocabulary[k]; ok {
			return true
		}
	}
	return false
}

// mappedCounters are the six keys this client actually reads. renamedCounter watches
// exactly these: a key the client never looks up can disappear without changing any
// number it reports.
var mappedCounters = []string{
	counterSent, counterDelivered, counterOpen, counterClick, counterBounce, counterUnsubscribed,
}

// renamedCounter reports a mapped key that is absent while at least one unrecognized key
// is present, returning the absent key and HOW MANY unrecognized keys there were. See
// ErrRenamedCounter for why NEITHER condition is sufficient alone.
//
// The unrecognized keys are COUNTED rather than returned: they are untrusted response
// content and the caller renders this into a logged error. Iteration over mapped keys is
// in declaration order and the count is order-independent, so the message is stable for a
// given input — one that varies run to run is one nobody can grep for.
func renamedCounter(counters map[string]int64) (missing string, unknownCount int, ok bool) {
	for _, k := range mappedCounters {
		if _, present := counters[k]; !present {
			missing = k
			break
		}
	}
	if missing == "" {
		return "", 0, false
	}
	for k := range counters {
		if _, known := knownCounterVocabulary[k]; !known {
			unknownCount++
		}
	}
	if unknownCount == 0 {
		return "", 0, false
	}
	return missing, unknownCount, true
}

// sortedKeys returns counters' keys in a stable order, so every message built by walking
// the map names the same key for the same input.
func sortedKeys(counters map[string]int64) []string {
	keys := make([]string, 0, len(counters))
	for k := range counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ratio returns num/den, and 0 when den is 0 — matching CampaignMetrics.Ctr's documented
// contract that it never divides by zero.
func ratio(num, den int64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// timeRangeForWindow converts a platform-agnostic window into the inclusive instant range
// HubSpot's statistics endpoint takes.
//
// Windows are anchored to the UTC calendar date — now() is converted with .UTC() before
// the date is extracted — so every caller gets the same range regardless of the process
// clock's zone. This matches the LinkedIn adapter's contract and is worth naming because
// HubSpot's own UI reports in the PORTAL's timezone: a portal set to America/Los_Angeles
// and a "today" read here can legitimately disagree at the day boundary. Choosing UTC
// keeps the answer identical across every platform in one dashboard, which is the property
// a cross-channel report needs; matching each portal's zone is not achievable anyway, since
// the ad platforms have their own.
//
// `end` is the last millisecond of the final day rather than midnight of the next. HubSpot
// does not document whether endTimestamp is inclusive; under either reading this range is
// wrong by at most one millisecond, whereas next-midnight would gain a whole extra day if
// the bound is inclusive.
//
// Month boundaries are computed from the first of the month (never AddDate(0,-1,0) on
// today's day-of-month), because AddDate normalizes an invalid day-of-month into the
// following month — which would shift this_month and last_month on the 29th-31st.
func (c *Client) timeRangeForWindow(window model.MetricsWindow) (start, end time.Time, err error) {
	// Checked first so this method and ValidateMetricsWindow can never disagree about which
	// windows are supported; TestValidateMetricsWindowMatchesTimeRangeForWindow pins that.
	if err := ValidateMetricsWindow(window); err != nil {
		return time.Time{}, time.Time{}, err
	}

	now := c.now().UTC()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	firstOfThisMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	var first, last time.Time
	switch window {
	case model.MetricsWindowToday:
		first, last = today, today
	case model.MetricsWindowYesterday:
		yesterday := today.AddDate(0, 0, -1)
		first, last = yesterday, yesterday
	case model.MetricsWindowLast7Days:
		first, last = today.AddDate(0, 0, -6), today // 6 days before today = 7 inclusive
	case model.MetricsWindowLast14Days:
		first, last = today.AddDate(0, 0, -13), today
	case model.MetricsWindowLast30Days:
		first, last = today.AddDate(0, 0, -29), today
	case model.MetricsWindowThisMonth:
		first, last = firstOfThisMonth, today
	case model.MetricsWindowLastMonth:
		// One day before the first of this month is always the last day of the previous
		// month, whatever its length.
		lastDayOfLastMonth := firstOfThisMonth.AddDate(0, 0, -1)
		first = time.Date(lastDayOfLastMonth.Year(), lastDayOfLastMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
		last = lastDayOfLastMonth
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}

	return first, last.Add(24*time.Hour - time.Millisecond), nil
}

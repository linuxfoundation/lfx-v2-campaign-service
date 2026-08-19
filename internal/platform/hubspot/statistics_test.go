// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fixedClock pins now() so window boundaries are assertable. 2026-03-15T12:00:00Z is
// deliberately UNREMARKABLE: mid-month, so every window it exercises has an in-range
// answer. It does NOT catch an AddDate(0, -1, 0) month-arithmetic bug — subtracting a
// month from the 15th is always valid — which is exactly why
// TestGetEmailMetrics_MonthWindowsOnAMonthEndDate pins its own clock to March 31.
func fixedClock(t *testing.T, c *Client) *Client {
	t.Helper()
	return clockAt(t, c, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
}

// clockAt pins now() to an arbitrary instant, for the cases whose whole point is the date.
func clockAt(t *testing.T, c *Client, at time.Time) *Client {
	t.Helper()
	withClock(func() time.Time { return at })(c)
	return c
}

// statsBody renders a statistics response with the given emails list and counters.
func statsBody(t *testing.T, emails string, counters string) string {
	t.Helper()
	return `{"emails":` + emails + `,"campaignAggregations":{},"aggregate":{"counters":` + counters +
		`,"ratios":{"openratio":0.5},"deviceBreakdown":{},"qualifierStats":{}}}`
}

const fullCounters = `{"sent":1000,"delivered":950,"open":400,"click":80,"bounce":50,"unsubscribed":7,"notsent":0}`

func TestGetEmailMetrics_MapsCountersAndComputesCtr(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if m.Impressions != 400 || m.Clicks != 80 {
		t.Errorf("impressions/clicks = %d/%d, want 400/80", m.Impressions, m.Clicks)
	}
	// 80/400. Computed here, NOT taken from the response's own openratio/clickratio, so
	// every platform's ctr means the same thing.
	if m.Ctr != 0.2 {
		t.Errorf("ctr = %v, want 0.2", m.Ctr)
	}
	if m.CostMicros != 0 {
		t.Errorf("cost_micros = %d, want 0 (email is not billed per send)", m.CostMicros)
	}
	if m.Window != model.MetricsWindowLast30Days {
		t.Errorf("window = %q", m.Window)
	}
	if m.Email == nil {
		t.Fatal("Email counters are nil; the email channel must populate them")
	}
	want := model.EmailMetrics{Sent: 1000, Delivered: 950, Opens: 400, Clicks: 80, Bounces: 50, Unsubscribes: 7}
	if *m.Email != want {
		t.Errorf("email = %+v, want %+v", *m.Email, want)
	}
}

func TestGetEmailMetrics_SendsTheWindowAsAnInclusiveUTCRange(t *testing.T) {
	// Every window, so a mis-mapped boundary is caught for all seven rather than the one
	// that happens to be exercised elsewhere. end is always the final millisecond of the
	// last day — see timeRangeForWindow on why not next-midnight.
	cases := []struct{ window, start, end string }{
		{"today", "2026-03-15T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"yesterday", "2026-03-14T00:00:00Z", "2026-03-14T23:59:59.999Z"},
		{"last_7_days", "2026-03-09T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"last_14_days", "2026-03-02T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"last_30_days", "2026-02-14T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"this_month", "2026-03-01T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		// February 2026 has 28 days. A month computed by subtracting one month from the
		// 15th would still land here, so the assertion that matters is the 28th end date.
		{"last_month", "2026-02-01T00:00:00Z", "2026-02-28T23:59:59.999Z"},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			// Guarded: httptest runs the handler on its own goroutine, so these writes and
			// the reads below are a data race even though the request has returned by then
			// — a happens-before the race detector cannot see.
			var mu sync.Mutex
			var gotStart, gotEnd, gotIDs, gotPath string
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = r.URL.Path
				q := r.URL.Query()
				gotStart, gotEnd, gotIDs = q.Get("startTimestamp"), q.Get("endTimestamp"), q.Get("emailIds")
				mu.Unlock()
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindow(tc.window)); err != nil {
				t.Fatalf("GetEmailMetrics: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotPath != statisticsPath {
				t.Errorf("path = %q, want %q", gotPath, statisticsPath)
			}
			if gotStart != tc.start || gotEnd != tc.end {
				t.Errorf("range = %s..%s, want %s..%s", gotStart, gotEnd, tc.start, tc.end)
			}
			if gotIDs != "4242" {
				t.Errorf("emailIds = %q, want 4242", gotIDs)
			}
		})
	}
}

// The response is unverifiable when its email list names something other than what we
// asked for: the filter was not honoured, so nothing in it describes this campaign.
// Reporting its counters would attribute a stranger's sends to this email.
func TestGetEmailMetrics_RejectsAResponseForADifferentEmail(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[9999]`, fullCounters))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrStatisticsFilterNotHonored) {
		t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
	}
}

// Company is a filter failure too, and this is the case a presence check waves through.
// The request names exactly one email and `aggregate` is the aggregation over the emails
// the response covers, so a list of three means the counters include two strangers'
// sends. Accepting it reports those sends as this campaign's — the same misattribution as
// accepting a response for the wrong email outright, just harder to notice.
func TestGetEmailMetrics_RejectsAResponseListingTheEmailAmongOthers(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[1,4242,9999]`, fullCounters))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrStatisticsFilterNotHonored) {
		t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
	}
}

// A REPEATED id is a filter failure the "every element matches" reading waves through.
// The request carries exactly one `emailIds` value, so [4242, 4242] is not an answer to
// it: nothing says the aggregate below sums one email's counters rather than two, and a
// response that mishandled the filter this way is no more trustworthy than one naming a
// stranger. The guard therefore asks for the singleton, not for agreement.
func TestGetEmailMetrics_RejectsADuplicatedEmailID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242,4242]`, fullCounters))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrStatisticsFilterNotHonored) {
		t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
	}
}

// The decode error is built from constants plus a length precisely so it can be logged,
// and nothing enforced that until now. json.UnmarshalTypeError copies an overflowing
// numeric literal into its own message, and BriefService.GetCampaignMetrics's default
// branch logs safeErrSummary(err), which truncates but does not redact — so a wrapped
// cause would put unvalidated upstream bytes in the server log. This is the same invariant
// internal/platform/linkedin/metrics_test.go pins for the sibling read.
func TestGetEmailMetrics_MalformedJSONIsRedacted(t *testing.T) {
	// The marker MUST be a numeric literal. For a string where an int64 was expected,
	// UnmarshalTypeError.Value is only the word "string" — the offending bytes never reach
	// it, so a quoted marker could not fail this assertion even against a verbatim wrap.
	const marker = "918273645509182736455091827364550"

	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"emails":[%s],"aggregate":{"counters":%s}}`, marker, fullCounters)
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("want an error: the emails list does not decode into []int64")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("err = %v, want the upstream literal absent — this string reaches the server log", err)
	}
	if !strings.Contains(err.Error(), "malformed JSON") {
		t.Errorf("err = %v, want the fixed diagnostic", err)
	}
}

// The absent-field half of the vocabulary guard. A renamed or dropped `counters` field
// decodes to a nil map, so `len(counters) > 0` never fires and every lookup returns 0 —
// an email HubSpot has just told us it covers reports as having sent nothing. The empty
// object and the missing key are the same break and both must be caught. Nothing licenses
// zeros here: an empty `emails` list is its own error, which
// TestGetEmailMetrics_EmptyEmailsListIsNotAZero pins from the other side.
func TestGetEmailMetrics_MissingCountersForACoveredEmailIsAnErrorNotZeros(t *testing.T) {
	for _, body := range []string{
		`{"emails":[4242],"aggregate":{"counters":{}}}`,
		`{"emails":[4242],"aggregate":{}}`,
		`{"emails":[4242]}`,
	} {
		t.Run(body, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if !errors.Is(err, ErrUnrecognizedCounters) {
				t.Fatalf("err = %v, want ErrUnrecognizedCounters", err)
			}
		})
	}
}

// An EMPTY emails list means HubSpot matched no SENT email with this id in the span. That
// is all it establishes: the span selects by SEND time, so a send outside it, a staged
// draft that has never been sent, and an id that does not exist all arrive in this one
// shape and the response cannot tell them apart. The assertions below hold the message to
// that weakest claim.
//
// Zeros are the wrong answer, and this test exists because the obvious reading — "empty
// means no activity" — gets it backwards. The email that really had no activity comes back
// PRESENT (see TestGetEmailMetrics_UnmappedButKnownCountersAreARealZero, where `[4242]`
// arrives carrying only `notsent`). So zeroing the empty case would make "no sent email
// matched" and "nobody opened it" the same answer, which is exactly the case where a live
// campaign reads as a dead one.
func TestGetEmailMetrics_EmptyEmailsListIsNotAZero(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[]`, `{}`))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrNoSentEmailInWindow) {
		t.Fatalf("err = %v, want ErrNoSentEmailInWindow", err)
	}
	if m != nil {
		t.Errorf("metrics = %+v, want nil: a partial result here would be read as real zeros", m)
	}
	// The sentinel must not narrow an empty list to "the send was outside the span". A
	// staged draft and a nonexistent id arrive in exactly this shape too, and a message
	// telling the caller to try another window is wrong for both of them.
	if !strings.Contains(err.Error(), "never sent") || !strings.Contains(err.Error(), "not exist") {
		t.Errorf("err = %v, want it to admit the other two states an empty list can mean", err)
	}
}

// The other half of that boundary. A response with NO `emails` field at all decodes to the
// same nil as `[]` if the field is a value slice, and the empty-list branch would then
// report a schema break as "wrong window" — sending the caller off to retry other windows
// against a response that can never carry what it needs. The field is the only evidence
// the filter was honoured, so its absence belongs with the filter guard.
func TestGetEmailMetrics_AbsentEmailsFieldIsNotAnEmptyList(t *testing.T) {
	for name, body := range map[string]string{
		"omitted": `{"aggregate":{"counters":{"sent":1000}}}`,
		"null":    `{"emails":null,"aggregate":{"counters":{"sent":1000}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if !errors.Is(err, ErrStatisticsFilterNotHonored) {
				t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
			}
			if errors.Is(err, ErrNoSentEmailInWindow) {
				t.Errorf("err = %v, must not read as a window miss: the field is absent, not empty", err)
			}
		})
	}
}

// The PARTIAL rename, which the whole-vocabulary guard below cannot see: `sent` survives,
// so at least one known key is present and that guard passes, while `open` has become
// `emailsOpened` and the mapped lookup returns an authoritative 0 for an email with 400
// opens. Both halves of the signature are required, and the two subtests that must NOT
// error are as much the point as the one that must — an omitted zero counter and an
// additive upstream key are ordinary, and rejecting either would break the client on a
// release that took nothing away.
func TestGetEmailMetrics_PartiallyRenamedCounterVocabularyIsAnError(t *testing.T) {
	tests := map[string]struct {
		counters string
		wantErr  bool
	}{
		"renamed open, sent intact":     {`{"sent":1000,"emailsOpened":400}`, true},
		"mapped key merely absent":      {`{"sent":1000,"open":400}`, false},
		"unknown key merely added":      {`{"sent":1,"delivered":1,"open":1,"click":1,"bounce":1,"unsubscribed":1,"engagements":9}`, false},
		"all mapped absent, only known": {`{"notsent":1}`, false},
		// The innocent co-occurrence: an upstream release adds `engagements` in the same
		// week this email omits its zero-valued `bounce`. It errors, and that is the
		// deliberate trade rather than an oversight — with both signals present the client
		// cannot distinguish this from a rename, and a false "cannot read" is recoverable
		// by widening the vocabulary while the false zero is a live campaign read as dead.
		"omission and addition together": {`{"sent":1,"delivered":1,"open":1,"click":1,"unsubscribed":1,"engagements":9}`, true},
		// The quiet path stays working however many mapped keys are absent: `pending` and
		// `notsent` are KNOWN, so there is no unknown key and the guard cannot fire.
		"quiet email, several known unmapped": {`{"notsent":1,"pending":2}`, false},
		// The sparse-but-legitimate shape the first probe set got wrong: a bounce
		// breakdown key from HubSpot's own v3 examples alongside an omitted zero-valued
		// mapped counter. Recognized now, so it is an ordinary success rather than a
		// rename. One subtest per newly-added key, because each was missing separately.
		"sparse with hardbounced":  {`{"sent":1,"delivered":1,"open":1,"click":1,"unsubscribed":1,"hardbounced":2}`, false},
		"sparse with softbounced":  {`{"sent":1,"delivered":1,"open":1,"click":1,"unsubscribed":1,"softbounced":2}`, false},
		"sparse with contactslost": {`{"sent":1,"delivered":1,"open":1,"click":1,"unsubscribed":1,"contactslost":2}`, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, tc.counters))
			})
			_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if got := errors.Is(err, ErrRenamedCounter); got != tc.wantErr {
				t.Fatalf("ErrRenamedCounter = %v, want %v (err = %v)", got, tc.wantErr, err)
			}
			// A non-error case has to be a SUCCESS, not merely a different failure. These
			// cases exist to prove an omitted zero counter and an additive upstream key
			// stay readable; a guard added elsewhere that rejected them would leave the
			// assertion above satisfied and the claim false.
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v, want nil: this shape must read successfully", err)
			}
		})
	}
}

// Counters are event counts, so a negative is malformed upstream data. Left unchecked it
// becomes a negative Impressions and a negative Ctr in the public response, both of which
// read as authoritative. LinkedIn, Meta and Reddit all reject negatives; this closes the
// same hole on HubSpot. The unmapped-key case is included deliberately: a negative
// anywhere is evidence the payload is wrong, and the keys we DO read are no more
// trustworthy for having stayed positive.
func TestGetEmailMetrics_NegativeCounterIsAnError(t *testing.T) {
	for _, counters := range []string{
		`{"sent":1000,"delivered":900,"open":-1,"click":10,"bounce":0,"unsubscribed":0}`,
		`{"sent":1000,"delivered":900,"open":5,"click":10,"bounce":0,"unsubscribed":0,"spamreport":-3}`,
	} {
		t.Run(counters, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, counters))
			})
			m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if !errors.Is(err, ErrNegativeCounter) {
				t.Fatalf("err = %v, want ErrNegativeCounter", err)
			}
			if m != nil {
				t.Errorf("metrics = %+v, want nil", m)
			}
		})
	}
}

// The redaction half of the negative-counter guard. Every key in the table above is in
// knownCounterVocabulary — including `spamreport` — so all of them take the branch that
// NAMES the key, and the branch that refuses to name one never ran. That branch is the
// log-safety one: an unrecognized key is arbitrary upstream content, and this error is
// rendered into a server log.
//
// The marker is chosen so the assertion cannot pass vacuously: it is long, unique, and
// would appear verbatim in `%q` output if the known-key branch were taken by mistake.
func TestGetEmailMetrics_NegativeUnrecognizedCounterIsNotNamed(t *testing.T) {
	const marker = "zz_unrecognized_counter_918273645509"

	// All six mapped keys stay present and non-negative, so the only thing wrong with
	// this response is the foreign key's sign — otherwise ErrRenamedCounter or
	// ErrUnrecognizedCounters could fire first and the negative branch would be skipped.
	body := fmt.Sprintf(
		`{"sent":1000,"delivered":900,"open":5,"click":10,"bounce":0,"unsubscribed":0,%q:-3}`, marker)

	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, body))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrNegativeCounter) {
		t.Fatalf("err = %v, want ErrNegativeCounter", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("err = %v, want the upstream key absent — this string reaches the server log", err)
	}
}

// The fail-closed guard. `counters` is an OPEN map in HubSpot's v3 schema, so a renamed
// key set decodes cleanly and every lookup returns 0 — an email that really sent would
// report as having sent nothing. That is indistinguishable from a dead campaign, so it
// must be an error instead.
func TestGetEmailMetrics_UnrecognizedCounterVocabularyIsAnErrorNotZeros(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, `{"emailsSent":1000,"emailsOpened":400}`))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrUnrecognizedCounters) {
		t.Fatalf("err = %v, want ErrUnrecognizedCounters", err)
	}
}

// The companion to the guard above, and the reason the probe set is wider than the six
// mapped keys: an email that WAS sent can come back carrying only counters this client
// does not map — every recipient still `pending`, or suppressed as `notsent` — with the
// six mapped counters zero and omitted. That is a real zero, not a vocabulary change.
// (An email never sent at all does not arrive here: `emails` would be empty and the call
// returns ErrNoSentEmailInWindow long before any counter is read.)
func TestGetEmailMetrics_UnmappedButKnownCountersAreARealZero(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, `{"notsent":12,"pending":3}`))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Email.Sent != 0 {
		t.Errorf("want zeros, got %+v / %+v", m, m.Email)
	}
}

// The counter names are not enumerated in HubSpot's v3 schema, so they are pinned here:
// an upstream rename should fail this test rather than silently zero a dashboard.
func TestCounterKeysAreTheDocumentedVocabulary(t *testing.T) {
	for name, got := range map[string]string{
		"sent": counterSent, "delivered": counterDelivered, "open": counterOpen,
		"click": counterClick, "bounce": counterBounce, "unsubscribed": counterUnsubscribed,
	} {
		if got != name {
			t.Errorf("counter constant for %q = %q", name, got)
		}
		if _, ok := knownCounterVocabulary[got]; !ok {
			t.Errorf("mapped counter %q is missing from knownCounterVocabulary, so a response carrying only it would be rejected", got)
		}
	}
}

// A malformed stored id is reported as malformed rather than answered with "no metrics":
// emailIds is an integer list upstream, so a non-integer would either be rejected there or
// ignored — and an ignored filter widens the query to the whole portal.
func TestGetEmailMetrics_RejectsAMalformedEmailIDBeforeAnyRequest(t *testing.T) {
	for _, id := range []string{"", "abc", "0", "-5", "+42", "042", " 42", "42 ", "42.0", "4242abc"} {
		t.Run(id, func(t *testing.T) {
			// atomic for the same reason as the range assertions: the handler runs on the
			// server's goroutine, and the assertion below is on the test's.
			var called atomic.Bool
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), id, model.MetricsWindowToday); err == nil {
				t.Fatalf("id %q was accepted", id)
			}
			if called.Load() {
				t.Error("a request was sent for a malformed id")
			}
		})
	}
}

// Pins that the validator and the range builder cannot disagree about which windows are
// supported — a window one accepts and the other rejects would surface as a 503 on a
// request the API surface advertises.
func TestValidateMetricsWindowMatchesTimeRangeForWindow(t *testing.T) {
	c := fixedClock(t, NewClient(testCreds(), testAccount()))
	for _, w := range []model.MetricsWindow{
		model.MetricsWindowToday, model.MetricsWindowYesterday, model.MetricsWindowLast7Days,
		model.MetricsWindowLast14Days, model.MetricsWindowLast30Days, model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth, model.MetricsWindow("not_a_window"), model.MetricsWindow(""),
	} {
		validErr := ValidateMetricsWindow(w)
		_, _, rangeErr := c.timeRangeForWindow(w)
		if (validErr == nil) != (rangeErr == nil) {
			t.Errorf("window %q: ValidateMetricsWindow=%v but timeRangeForWindow=%v", w, validErr, rangeErr)
		}
	}
}

// supportedMetricsWindows is HubSpot's OWN enumeration, not a view of the shared model's.
// The two happen to be equal today, which is exactly why the distinction needs pinning
// somewhere: with equal sets, re-delegating to model.IsValidMetricsWindow would pass every
// other test in this file while reintroducing the fail-open behaviour — a window added to
// the shared vocabulary and not mapped here would validate, resolve credentials, and only
// then fail. This test asserts membership exactly, so an addition to the model that is
// mirrored here has to be a deliberate edit to this list rather than a silent inheritance.
func TestSupportedMetricsWindowsIsHubspotsOwnEnumeration(t *testing.T) {
	want := []model.MetricsWindow{
		model.MetricsWindowToday, model.MetricsWindowYesterday, model.MetricsWindowLast7Days,
		model.MetricsWindowLast14Days, model.MetricsWindowLast30Days, model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth,
	}
	if len(supportedMetricsWindows) != len(want) {
		t.Fatalf("supportedMetricsWindows has %d entries, want %d", len(supportedMetricsWindows), len(want))
	}
	for _, w := range want {
		if _, ok := supportedMetricsWindows[w]; !ok {
			t.Errorf("window %q is mapped by timeRangeForWindow but missing from supportedMetricsWindows", w)
		}
	}
}

// The whole shared vocabulary is supported, because HubSpot takes arbitrary instants. A
// future window added to model without a case here must fail, not fall through.
func TestEveryMetricsWindowIsSupported(t *testing.T) {
	c := fixedClock(t, NewClient(testCreds(), testAccount()))
	for _, w := range []model.MetricsWindow{
		model.MetricsWindowToday, model.MetricsWindowYesterday, model.MetricsWindowLast7Days,
		model.MetricsWindowLast14Days, model.MetricsWindowLast30Days, model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth,
	} {
		start, end, err := c.timeRangeForWindow(w)
		if err != nil {
			t.Errorf("window %q: %v", w, err)
			continue
		}
		if !start.Before(end) {
			t.Errorf("window %q: start %s is not before end %s", w, start, end)
		}
	}
}

// A 429 on this read is retried (idempotent=true), which is what distinguishes it from the
// clone path. Asserted through the public method so a future change to the idempotent flag
// at the call site is caught.
func TestGetEmailMetrics_RetriesA429(t *testing.T) {
	// atomic, and not merely because of the assertion at the end: the two retry attempts
	// are separate handler invocations that the server may run on different goroutines, so
	// the read-modify-write itself is the race.
	var attempts atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
	})
	if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday); err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

// TestGetEmailMetrics_MonthWindowsOnAMonthEndDate is the case the 2026-03-15 fixture
// cannot make: subtracting a month from the 15th is always valid, so a naive
// AddDate(0, -1, 0) would pass every assertion above.
//
// On the 31st it does not. AddDate NORMALIZES an out-of-range day — 2026-03-31 minus one
// month is 2026-02-31, which becomes 2026-03-03 — so the naive form would report last_month
// as a few days of MARCH, and this_month's start would follow it out of the month entirely.
// timeRangeForWindow avoids that by computing both from the first of the month; this pins
// that it still does.
func TestGetEmailMetrics_MonthWindowsOnAMonthEndDate(t *testing.T) {
	cases := []struct{ window, start, end string }{
		{"this_month", "2026-03-01T00:00:00Z", "2026-03-31T23:59:59.999Z"},
		{"last_month", "2026-02-01T00:00:00Z", "2026-02-28T23:59:59.999Z"},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			var mu sync.Mutex
			var gotStart, gotEnd string
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				q := r.URL.Query()
				gotStart, gotEnd = q.Get("startTimestamp"), q.Get("endTimestamp")
				mu.Unlock()
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			onTheLastOfMarch := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
			if _, err := clockAt(t, c, onTheLastOfMarch).GetEmailMetrics(
				context.Background(), "4242", model.MetricsWindow(tc.window)); err != nil {
				t.Fatalf("GetEmailMetrics: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotStart != tc.start || gotEnd != tc.end {
				t.Errorf("range = %s..%s, want %s..%s", gotStart, gotEnd, tc.start, tc.end)
			}
		})
	}
}

// The email channel reports NO conversion count, and the honest representation of that is
// absence — not zero.
//
// The contrast with CostMicros in the same struct is the whole point, and this test asserts
// both together so the difference cannot be "simplified" away. CostMicros=0 is a MEASUREMENT:
// HubSpot genuinely bills nothing per send, so zero is the true cost. A conversion count has
// no true value here at all — the statistics endpoint's counter vocabulary contains no
// conversion counter, because a marketing email send has no campaign-level conversion concept.
// Writing 0 would claim this email converted nobody, and the no_conversions rule would then
// flag every email campaign in the account forever.
func TestGetEmailMetrics_ConversionsAbsentNotZero(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if m.Conversions != nil {
		t.Errorf("Conversions = %d on the email channel, which measures no conversions at "+
			"all; a fabricated count would make every email campaign look like it converted "+
			"nobody", *m.Conversions)
	}
	// Pinned alongside so the two absences stay distinguishable: this 0 IS a measurement.
	if m.CostMicros != 0 {
		t.Errorf("cost_micros = %d, want a measured 0", m.CostMicros)
	}
}

// The counter vocabulary this client recognises carries no conversion key. If HubSpot ever
// adds one, this test is where the claim above stops being true — it fails, and the adapter
// can be revisited deliberately rather than the absence quietly persisting as a lie.
func TestCounterVocabularyHasNoConversionCounter(t *testing.T) {
	for name := range knownCounterVocabulary {
		if strings.Contains(strings.ToLower(name), "conversion") {
			t.Errorf("the recognised counter vocabulary now contains %q: HubSpot may report "+
				"conversions after all, so CampaignMetrics.Conversions should no longer be "+
				"left absent for the email channel", name)
		}
	}
}

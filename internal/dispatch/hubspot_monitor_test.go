// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.EmailMonitorReader = (*HubSpotDispatcher)(nil)

const statisticsWirePath = "/marketing/v3/emails/statistics/list"

// monitorStats renders a statistics response for one email id with the given counters.
func monitorStats(id, counters string) string {
	return `{"emails":[` + id + `],"campaignAggregations":{},"aggregate":{"counters":` + counters + `}}`
}

// monitorRec is what the fake HubSpot saw. Written by handler goroutines (several at once —
// the dispatcher reads emails concurrently) and read by the test goroutine after the call
// returns, so every access is under mu.
type monitorRec struct {
	mu          sync.Mutex
	statsIDs    []string
	tokenCalls  int
	inFlight    int
	maxInFlight int
}

func (r *monitorRec) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string{}, r.statsIDs...)
	sort.Strings(out)
	return out
}

func (r *monitorRec) tokens() int { r.mu.Lock(); defer r.mu.Unlock(); return r.tokenCalls }
func (r *monitorRec) peak() int   { r.mu.Lock(); defer r.mu.Unlock(); return r.maxInFlight }

// monitorServer answers token-info with the test portal and each statistics request from
// byID (keyed by the emailIds value); an id missing from byID answers an empty `emails`
// list — HubSpot's "no send of this email in the span". status, when set for an id, is
// returned instead of a body. hold, when non-nil, delays every statistics answer until it is
// closed, so concurrent requests overlap deterministically.
type monitorServerOpts struct {
	byID        map[string]string
	status      map[string]int
	tokenStatus int
	hold        chan struct{}
}

func monitorServer(t *testing.T, o monitorServerOpts) (*httptest.Server, *monitorRec) {
	t.Helper()
	rec := &monitorRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case accountDetailsPath:
			rec.mu.Lock()
			rec.tokenCalls++
			rec.mu.Unlock()
			if o.tokenStatus != 0 {
				w.WriteHeader(o.tokenStatus)
				return
			}
			_, _ = io.WriteString(w, testPortalResponse)
		case statisticsWirePath:
			id := r.URL.Query().Get("emailIds")
			rec.mu.Lock()
			rec.statsIDs = append(rec.statsIDs, id)
			rec.inFlight++
			if rec.inFlight > rec.maxInFlight {
				rec.maxInFlight = rec.inFlight
			}
			rec.mu.Unlock()
			defer func() { rec.mu.Lock(); rec.inFlight--; rec.mu.Unlock() }()
			if o.hold != nil {
				select {
				case <-o.hold:
				case <-r.Context().Done():
					return
				}
			}
			if st, ok := o.status[id]; ok {
				w.WriteHeader(st)
				return
			}
			body, ok := o.byID[id]
			if !ok {
				body = `{"emails":[],"campaignAggregations":{},"aggregate":{"counters":{}}}`
			}
			_, _ = io.WriteString(w, body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// recordedEmail is a HubSpot campaign row as campaignFromHubSpot writes it: the email id as the
// platform campaign id and the creating portal (and any A/B variant) in Result.
func recordedEmail(id, emailID, portal string, variant *hubspot.Email) *model.Campaign {
	blob := map[string]any{"id": emailID}
	if portal != "" {
		blob["portalId"] = portal
	}
	if variant != nil {
		blob["abTestVariant"] = variant
	}
	raw, _ := json.Marshal(blob)
	return &model.Campaign{
		ID: id, Platform: model.ProviderHubSpot, PlatformCampaignID: emailID,
		CampaignName: "email " + emailID, Result: raw,
	}
}

func newMonitorDispatcher(srv *httptest.Server, reader connReader) *HubSpotDispatcher {
	return NewHubSpotDispatcher(reader, identityEncryptor{}, fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL))
}

func TestHubSpotEmailMonitor_ReadsEachRecordedEmailAndCountsTheRest(t *testing.T) {
	srv, rec := monitorServer(t, monitorServerOpts{byID: map[string]string{
		"101": monitorStats("101", `{"sent":1000,"delivered":980,"open":300,"click":40,"bounce":20,"unsubscribed":3,"spamreport":1}`),
		"102": monitorStats("102", `{"sent":500,"delivered":490,"open":100,"click":10,"bounce":10,"unsubscribed":1}`),
	}})
	d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
	campaigns := []*model.Campaign{
		recordedEmail("c1", "101", testPortalID, &hubspot.Email{ID: "102", Name: "email 101 - Variant B"}),
		recordedEmail("c2", "103", testPortalID, nil), // not sent in the span
	}

	read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, campaigns, 30)
	if err != nil {
		t.Fatalf("ReadEmailMonitor: %v", err)
	}
	if got := rec.ids(); len(got) != 3 || got[0] != "101" || got[1] != "102" || got[2] != "103" {
		t.Fatalf("statistics requested for %v, want exactly 101, 102, 103 — one request per recorded email", got)
	}
	if read.EmailsChecked != 3 || read.EmailsNotSentInWindow != 1 || read.EmailsUnattributable != 0 {
		t.Errorf("checked/not-sent/unattributable = %d/%d/%d, want 3/1/0",
			read.EmailsChecked, read.EmailsNotSentInWindow, read.EmailsUnattributable)
	}
	if len(read.Emails) != 2 {
		t.Fatalf("emails = %+v, want the two sent ones", read.Emails)
	}
	parent, variant := read.Emails[0], read.Emails[1]
	if parent.CampaignID != "c1" || parent.EmailID != "101" || parent.ABVariant {
		t.Errorf("parent row = %+v", parent)
	}
	if parent.Counters != (model.HubSpotEmailCounters{Sent: 1000, Delivered: 980, Opens: 300, Clicks: 40, Bounces: 20, Unsubscribes: 3, SpamReports: 1}) {
		t.Errorf("parent counters = %+v", parent.Counters)
	}
	if variant.CampaignID != "c1" || variant.EmailID != "102" || !variant.ABVariant || variant.Name != "email 101 - Variant B" {
		t.Errorf("variant row = %+v", variant)
	}
	if variant.Counters.SpamReports != 0 {
		t.Errorf("an absent spamreport counter must read as 0 (an omitted zero), got %d", variant.Counters.SpamReports)
	}
	if read.SpanStart.IsZero() || read.SpanEnd.IsZero() || read.AsOf.IsZero() {
		t.Errorf("span/as-of not set on a read that called HubSpot: %+v", read)
	}
	if rec.tokens() != 1 {
		t.Errorf("token-info called %d times, want once per read", rec.tokens())
	}
}

// An email id means something only inside the portal that minted it. A row that records no
// portal, another portal, or a malformed id is counted and NEVER sent upstream.
func TestHubSpotEmailMonitor_UnattributableEmailsAreNotRead(t *testing.T) {
	srv, rec := monitorServer(t, monitorServerOpts{byID: map[string]string{
		"201": monitorStats("201", `{"sent":10,"delivered":10}`),
	}})
	d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
	campaigns := []*model.Campaign{
		recordedEmail("c1", "201", testPortalID, nil),
		recordedEmail("c2", "202", "", nil),                             // provenance unknown
		recordedEmail("c3", "203", "999999", &hubspot.Email{ID: "204"}), // another portal: both emails
		recordedEmail("c4", "0205", testPortalID, nil),                  // non-canonical id
	}
	read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, campaigns, 7)
	if err != nil {
		t.Fatalf("ReadEmailMonitor: %v", err)
	}
	if got := rec.ids(); len(got) != 1 || got[0] != "201" {
		t.Fatalf("statistics requested for %v, want only the attributable 201", got)
	}
	if read.EmailsUnattributable != 4 || read.EmailsChecked != 1 || len(read.Emails) != 1 {
		t.Errorf("unattributable/checked/emails = %d/%d/%d, want 4/1/1", read.EmailsUnattributable, read.EmailsChecked, len(read.Emails))
	}
}

// The same email recorded on two rows is read once, under the first (newest) row.
func TestHubSpotEmailMonitor_DuplicateEmailIsReadOnce(t *testing.T) {
	srv, rec := monitorServer(t, monitorServerOpts{byID: map[string]string{
		"301": monitorStats("301", `{"sent":10,"delivered":10}`),
	}})
	d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
	read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, []*model.Campaign{
		recordedEmail("newer", "301", testPortalID, nil),
		recordedEmail("older", "301", testPortalID, nil),
	}, 30)
	if err != nil {
		t.Fatalf("ReadEmailMonitor: %v", err)
	}
	if got := rec.ids(); len(got) != 1 {
		t.Fatalf("statistics requested %d times for one email", len(got))
	}
	if len(read.Emails) != 1 || read.Emails[0].CampaignID != "newer" {
		t.Errorf("emails = %+v, want one row under the newer campaign", read.Emails)
	}
}

// HubSpot email ids are unique only within a portal. A NEWER row recorded against another portal
// (or with a malformed id) carrying the same number must not suppress the older attributable row,
// and must itself be counted unattributable — in either order.
func TestHubSpotEmailMonitor_AttributionIsDecidedBeforeDedupe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []*model.Campaign
		wantC string
	}{
		{"foreign newer, attributable older", []*model.Campaign{
			recordedEmail("foreign", "801", "999999", nil), recordedEmail("ours", "801", testPortalID, nil)}, "ours"},
		{"attributable newer, foreign older", []*model.Campaign{
			recordedEmail("ours", "801", testPortalID, nil), recordedEmail("foreign", "801", "999999", nil)}, "ours"},
		{"unrecorded-portal newer, attributable older", []*model.Campaign{
			recordedEmail("legacy", "801", "", nil), recordedEmail("ours", "801", testPortalID, nil)}, "ours"},
		{"malformed variant newer, attributable older", []*model.Campaign{
			recordedEmail("bad", "0801", testPortalID, nil), recordedEmail("ours", "801", testPortalID, nil)}, "ours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := monitorServer(t, monitorServerOpts{byID: map[string]string{
				"801": monitorStats("801", `{"sent":10,"delivered":10}`),
			}})
			d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
			read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, tc.rows, 30)
			if err != nil {
				t.Fatalf("ReadEmailMonitor: %v", err)
			}
			if len(read.Emails) != 1 || read.Emails[0].CampaignID != tc.wantC {
				t.Errorf("emails = %+v, want one row under %q", read.Emails, tc.wantC)
			}
			if read.EmailsUnattributable != 1 {
				t.Errorf("unattributable = %d, want 1 (the other row)", read.EmailsUnattributable)
			}
			if got := rec.ids(); len(got) != 1 || got[0] != "801" {
				t.Errorf("statistics requested for %v, want 801 once", got)
			}
		})
	}
}

// DEFINITE OR NOTHING: one email's failure fails the read, whatever the others returned.
func TestHubSpotEmailMonitor_AnyUpstreamFailureFailsTheWholeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts monitorServerOpts
	}{
		{"401 on one email", monitorServerOpts{status: map[string]int{"402": http.StatusUnauthorized}}},
		{"403 on one email", monitorServerOpts{status: map[string]int{"402": http.StatusForbidden}}},
		{"5xx on one email", monitorServerOpts{status: map[string]int{"402": http.StatusBadGateway}}},
		{"429 that never clears", monitorServerOpts{status: map[string]int{"402": http.StatusTooManyRequests}}},
		{"duplicate key", monitorServerOpts{byID: map[string]string{"402": `{"emails":[402],"aggregate":{"counters":{"sent":10,"sent":0}}}`}}},
		{"null counter", monitorServerOpts{byID: map[string]string{"402": monitorStats("402", `{"sent":10,"bounce":null}`)}}},
		{"malformed JSON", monitorServerOpts{byID: map[string]string{"402": `{"emails":[402],`}}},
		{"filter not honoured", monitorServerOpts{byID: map[string]string{"402": monitorStats("402, 999", `{"sent":10}`)}}},
		{"token-info refused", monitorServerOpts{tokenStatus: http.StatusUnauthorized}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.opts.byID == nil {
				tc.opts.byID = map[string]string{}
			}
			tc.opts.byID["401"] = monitorStats("401", `{"sent":10,"delivered":10}`)
			if _, ok := tc.opts.byID["402"]; !ok {
				tc.opts.byID["402"] = monitorStats("402", `{"sent":10,"delivered":10}`)
			}
			srv, _ := monitorServer(t, tc.opts)
			d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			read, err := d.ReadEmailMonitor(ctx, "proj-1", model.ProviderHubSpot, []*model.Campaign{
				recordedEmail("c1", "401", testPortalID, nil),
				recordedEmail("c2", "402", testPortalID, nil),
			}, 30)
			if err == nil {
				t.Fatalf("a failed upstream read returned a result: %+v", read)
			}
			if read != nil {
				t.Errorf("a partial result accompanied the error: %+v", read)
			}
			// A 401/403 is a credential the platform refused (tested in
			// TestHubSpotEmailMonitor_PermissionRejectionsAreConnectionDefects); every other
			// failure must stay unclassified, i.e. the retryable 503.
			auth := strings.HasPrefix(tc.name, "401") || strings.HasPrefix(tc.name, "403") || tc.name == "token-info refused"
			if errors.Is(err, domain.ErrNotFound) || (!auth && errors.Is(err, domain.ErrConnectionNotUsable)) {
				t.Errorf("an upstream failure was classified as a connection state: %v", err)
			}
		})
	}
}

// A 401/403 on token-info or statistics is a credential HubSpot refused: a revoked token or a
// missing scope does not recover by retrying. Tagged like SearchEmails/SearchCampaigns: the
// project's own token → ErrConnectionNotUsable (400); the LF system fallback token → the
// operator-owned row's defect, ErrSystemConnectionNotUsable (500).
func TestHubSpotEmailMonitor_PermissionRejectionsAreConnectionDefects(t *testing.T) {
	for _, call := range []struct {
		name string
		opts monitorServerOpts
	}{
		{"token-info 401", monitorServerOpts{tokenStatus: http.StatusUnauthorized}},
		{"statistics 403", monitorServerOpts{status: map[string]int{"901": http.StatusForbidden}}},
	} {
		for _, origin := range []struct {
			name   string
			reader connReader
			system bool
		}{
			{"project-owned token", fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, false},
			{"LF system fallback token", ownOnlyConnReader{system: activeHubSpotConn(goodHubSpotCreds)}, true},
		} {
			t.Run(call.name+"/"+origin.name, func(t *testing.T) {
				srv, _ := monitorServer(t, call.opts)
				d := newMonitorDispatcher(srv, origin.reader)
				read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot,
					[]*model.Campaign{recordedEmail("c1", "901", testPortalID, nil)}, 30)
				if read != nil {
					t.Fatalf("a partial result accompanied the refusal: %+v", read)
				}
				if !errors.Is(err, domain.ErrConnectionNotUsable) {
					t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
				}
				if got := errors.Is(err, domain.ErrSystemConnectionNotUsable); got != origin.system {
					t.Errorf("attributed to the LF system row = %v, want %v: %v", got, origin.system, err)
				}
			})
		}
	}
}

// metrics_as_of is the instant the LAST upstream response arrived — every counter was read at or
// before it — not the instant the read started. The pinned clock advances one minute per reading.
func TestHubSpotEmailMonitor_AsOfIsWhenTheLastResponseArrived(t *testing.T) {
	srv, _ := monitorServer(t, monitorServerOpts{byID: map[string]string{
		"1001": monitorStats("1001", `{"sent":1}`), "1002": monitorStats("1002", `{"sent":1}`),
	}})
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	tick := 0
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick++
		return base.Add(time.Duration(tick) * time.Minute)
	}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL), hubspot.WithClock(clock))
	read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, []*model.Campaign{
		recordedEmail("c1", "1001", testPortalID, nil), recordedEmail("c2", "1002", testPortalID, nil),
	}, 30)
	if err != nil {
		t.Fatalf("ReadEmailMonitor: %v", err)
	}
	mu.Lock()
	last := base.Add(time.Duration(tick) * time.Minute)
	mu.Unlock()
	// The clock was read for the span, after token-info and after each of the two statistics
	// responses; as-of must be the final reading, not the span's.
	if !read.AsOf.Equal(last) {
		t.Errorf("as-of = %s, want the last reading %s (after both statistics responses)", read.AsOf, last)
	}
	if tick < 4 {
		t.Errorf("the clock was read %d times; want span, token-info and one per statistics response", tick)
	}
}

// The token-info failure is decided before any statistics request: without the portal no email
// can be attributed, so none is read.
func TestHubSpotEmailMonitor_TokenInfoFailureSendsNoStatisticsRequest(t *testing.T) {
	srv, rec := monitorServer(t, monitorServerOpts{tokenStatus: http.StatusForbidden})
	d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
	if _, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot,
		[]*model.Campaign{recordedEmail("c1", "501", testPortalID, nil)}, 30); err == nil {
		t.Fatal("a refused token-info lookup was not an error")
	}
	if got := rec.ids(); len(got) != 0 {
		t.Errorf("statistics requested for %v after token-info failed", got)
	}
}

// days is re-checked before any credential is resolved: the missing connection here would
// otherwise answer instead of the bad request.
func TestHubSpotEmailMonitor_ValidatesDaysBeforeResolvingCredentials(t *testing.T) {
	d := NewHubSpotDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, fakeAudienceReader{})
	_, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, nil, 6)
	if !errors.Is(err, domain.ErrMonitorDaysInvalid) {
		t.Fatalf("err = %v, want ErrMonitorDaysInvalid", err)
	}
}

// ownOnlyConnReader serves a connection for the LF system project only (none when system is nil),
// so a resolver that takes the system fallback succeeds and one that refuses it reports ErrNotFound.
type ownOnlyConnReader struct{ system *model.Connection }

func (r ownOnlyConnReader) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	if projectID == model.SystemProjectID && r.system != nil {
		return r.system, nil
	}
	return nil, domain.ErrNotFound
}
func (ownOnlyConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// The connection resolves like Dispatch and ReadMetrics: a project whose emails went through the LF
// system connection (it has none of its own) is monitored on the system portal, and the per-email
// portal check — not the resolver — is the boundary: a row recorded against another portal is
// still unattributable and never read.
func TestHubSpotEmailMonitor_SystemFallbackIsMonitoredAndPortalCheckStillApplies(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary fallback", true: "forced-system mode"}[forced], func(t *testing.T) {
			srv, rec := monitorServer(t, monitorServerOpts{byID: map[string]string{
				"601": monitorStats("601", `{"sent":10,"delivered":10}`),
			}})
			d := newMonitorDispatcher(srv, ownOnlyConnReader{system: activeHubSpotConn(goodHubSpotCreds)})
			d.creds.forceSystemPaidAds = forced
			read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, []*model.Campaign{
				recordedEmail("c1", "601", testPortalID, nil),
				recordedEmail("c2", "602", "999999", nil),
			}, 30)
			if err != nil {
				t.Fatalf("ReadEmailMonitor on the LF system connection: %v", err)
			}
			if len(read.Emails) != 1 || read.Emails[0].EmailID != "601" || read.EmailsUnattributable != 1 {
				t.Errorf("emails/unattributable = %+v/%d, want 601 read and the other-portal row counted", read.Emails, read.EmailsUnattributable)
			}
			if got := rec.ids(); len(got) != 1 || got[0] != "601" {
				t.Errorf("statistics requested for %v, want only 601", got)
			}
		})
	}
}

// With no connection of its own AND no LF system row, the read is ErrNotFound (404) and HubSpot
// is not contacted.
func TestHubSpotEmailMonitor_NoConnectionAtAllIsNotFound(t *testing.T) {
	srv, rec := monitorServer(t, monitorServerOpts{})
	d := newMonitorDispatcher(srv, ownOnlyConnReader{})
	_, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot,
		[]*model.Campaign{recordedEmail("c1", "601", testPortalID, nil)}, 30)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if rec.tokens() != 0 || len(rec.ids()) != 0 {
		t.Error("HubSpot was contacted with no connection")
	}
}

// A portal throttled past the client's retries (shared-app contention) fails the whole read: no
// partial result, and nothing a classifier would map to anything but the 503 default.
func TestHubSpotEmailMonitor_ThrottledPastRetriesFailsTheWholeRead(t *testing.T) {
	srv, _ := monitorServer(t, monitorServerOpts{
		byID:   map[string]string{"701": monitorStats("701", `{"sent":10,"delivered":10}`)},
		status: map[string]int{"702": http.StatusTooManyRequests},
	})
	d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})
	read, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, []*model.Campaign{
		recordedEmail("c1", "701", testPortalID, nil), recordedEmail("c2", "702", testPortalID, nil),
	}, 30)
	if err == nil || read != nil {
		t.Fatalf("read=%+v err=%v; want no result and an error", read, err)
	}
	for _, s := range []error{domain.ErrNotFound, domain.ErrConnectionNotUsable, domain.ErrCredentialDecryptionFailed,
		domain.ErrServiceDefect, domain.ErrMonitorDaysInvalid, domain.ErrSystemConnectionNotUsable} {
		if errors.Is(err, s) {
			t.Errorf("a throttled read carries %v, which would not classify as 503", s)
		}
	}
}

// At most hubspotMonitorConcurrency statistics requests are in flight. The handler holds every
// request until released, so all the dispatcher is willing to send overlap before any returns.
func TestHubSpotEmailMonitor_BoundsConcurrency(t *testing.T) {
	hold := make(chan struct{})
	byID := map[string]string{}
	campaigns := make([]*model.Campaign, 0, 10)
	for i := 0; i < 10; i++ {
		id := string(rune('1'+i%9)) + "00" + string(rune('0'+i))
		byID[id] = monitorStats(id, `{"sent":1}`)
		campaigns = append(campaigns, recordedEmail("c"+id, id, testPortalID, nil))
	}
	srv, rec := monitorServer(t, monitorServerOpts{byID: byID, hold: hold})
	d := newMonitorDispatcher(srv, fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)})

	done := make(chan error, 1)
	go func() {
		_, err := d.ReadEmailMonitor(context.Background(), "proj-1", model.ProviderHubSpot, campaigns, 30)
		done <- err
	}()
	// Wait until the dispatcher has as many requests in flight as it will send, then release.
	deadline := time.After(10 * time.Second)
	for rec.peak() < hubspotMonitorConcurrency {
		select {
		case <-deadline:
			t.Fatalf("only %d requests ever in flight", rec.peak())
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("ReadEmailMonitor: %v", err)
	}
	if p := rec.peak(); p > hubspotMonitorConcurrency {
		t.Errorf("%d statistics requests in flight at once, want at most %d", p, hubspotMonitorConcurrency)
	}
	if got := len(rec.ids()); got != 10 {
		t.Errorf("%d statistics requests, want 10", got)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// audienceNow is the pinned clock: 12:00 UTC on 2026-10-05, which is 05:00 on Oct 5 in
// America/Los_Angeles (PDT, UTC-7).
var audienceNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// audienceCanary is planted in upstream bodies; no error may carry it.
const audienceCanary = "CANARY<script>"

// fakeXAudience is a stateful X Ads stub for the audience read. Every handler records what it
// served under mu BEFORE writing its response, so once the client call returns every request it
// made is visible to the test — the synchronized handoff between the server goroutines and the
// test goroutine.
type fakeXAudience struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// account is the GET accounts/:id body.
	account string
	// createStatus, when non-zero, answers every job POST with that status.
	createStatus int
	// createBody, when set, replaces the job POST body ("%s" receives the new job id).
	createBody string
	// pendingReads is how many status reads answer PROCESSING before SUCCESS; < 0 means never.
	pendingReads int
	// statusBody, when set, replaces the status read body.
	statusBody func(ids []string) string
	// statusReject, when set, answers every status read instead of the normal body.
	statusReject func(w http.ResponseWriter)
	// file builds a job's results file from its segmentation and entity ids.
	file func(segmentation string, ids []string) string

	posts       []url.Values
	statusReads int
	accountGets int
	jobs        map[string]url.Values
	nextJob     int
}

func newFakeXAudience(t *testing.T) *fakeXAudience {
	t.Helper()
	f := &fakeXAudience{
		t:       t,
		account: `{"data":{"id":"account123","timezone":"America/Los_Angeles","currency":"EUR"}}`,
		file:    defaultAudienceFile,
		jobs:    map[string]url.Values{},
		nextJob: 1000,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeXAudience) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/12/accounts/account123":
		f.accountGets++
		_, _ = fmt.Fprint(w, f.account)
	case r.Method == http.MethodPost && r.URL.Path == "/12/stats/jobs/accounts/account123":
		f.posts = append(f.posts, r.URL.Query())
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			_, _ = fmt.Fprintf(w, `{"errors":[{"code":"X","message":%q}]}`, audienceCanary)
			return
		}
		id := strconv.Itoa(f.nextJob)
		f.nextJob++
		f.jobs[id] = r.URL.Query()
		if f.createBody != "" {
			_, _ = fmt.Fprintf(w, f.createBody, id)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id_str":%q,"status":"PROCESSING"}}`, id)
	case r.Method == http.MethodGet && r.URL.Path == "/12/stats/jobs/accounts/account123":
		f.statusReads++
		ids := strings.Split(r.URL.Query().Get("job_ids"), ",")
		if f.statusReject != nil {
			f.statusReject(w)
			return
		}
		if f.statusBody != nil {
			_, _ = fmt.Fprint(w, f.statusBody(ids))
			return
		}
		els := make([]string, 0, len(ids))
		for _, id := range ids {
			if f.pendingReads < 0 || f.statusReads <= f.pendingReads {
				els = append(els, fmt.Sprintf(`{"id_str":%q,"status":"PROCESSING","url":null}`, id))
				continue
			}
			els = append(els, fmt.Sprintf(`{"id_str":%q,"status":"SUCCESS","url":%q}`, id, f.srv.URL+"/files/"+id+".json.gz"))
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(els, ","))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/files/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), ".json.gz")
		q, ok := f.jobs[id]
		if !ok {
			f.t.Errorf("download of unknown job %s", id)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(gz(f.t, f.file(q.Get("segmentation_type"), strings.Split(q.Get("entity_ids"), ","))))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeXAudience) set(fn func(*fakeXAudience)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeXAudience) snapshot() (posts []url.Values, statusReads, accountGets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.posts...), f.statusReads, f.accountGets
}

func (f *fakeXAudience) client(now time.Time) *Client {
	return NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(f.srv.URL), WithWriteDelay(0), WithClock(func() time.Time { return now }),
		withAudiencePollInterval(time.Millisecond),
	)
}

// audienceSegments are the segment names the default file reports per segmentation.
var audienceSegments = map[string][]string{
	"AGE":       {"18-24", "25-34"},
	"GENDER":    {"Male", "Female"},
	"PLATFORMS": {"iOS", "Desktop and laptop computers"},
}

// defaultAudienceFile gives every campaign, in every segment, impressions 100*(k+1), clicks
// 5*(k+1) and billed charge 1_000_000*(k+1), where k is the segment's index.
func defaultAudienceFile(segmentation string, ids []string) string {
	ents := make([]string, 0, len(ids))
	for _, id := range ids {
		segs := make([]string, 0, 2)
		for k, name := range audienceSegments[segmentation] {
			n := int64(k + 1)
			segs = append(segs, fmt.Sprintf(`{"segment":{"segment_name":%q,"segment_value":"v%d"},"metrics":{"impressions":[%d],"clicks":[%d],"billed_charge_local_micro":[%d]}}`,
				name, k, 100*n, 5*n, 1_000_000*n))
		}
		ents = append(ents, fmt.Sprintf(`{"id":%q,"id_data":[%s]}`, id, strings.Join(segs, ",")))
	}
	return fmt.Sprintf(`{"data_type":"stats","data":[%s],"request":{"params":{}}}`, strings.Join(ents, ","))
}

func audienceIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "c"+strconv.Itoa(i))
	}
	return out
}

// One job per segmentation, each carrying the documented parameter set, and buckets summed per
// segment across campaigns with CTR computed after summing.
func TestGetAudienceInsights_SegmentationRequestsAndSums(t *testing.T) {
	f := newFakeXAudience(t)
	ai, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowLast7Days, []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	posts, _, _ := f.snapshot()
	if len(posts) != 3 {
		t.Fatalf("%d job POSTs, want 3 (one per segmentation)", len(posts))
	}
	for i, want := range []string{"AGE", "GENDER", "PLATFORMS"} {
		q := posts[i]
		for k, v := range map[string]string{
			"segmentation_type": want, "entity": "CAMPAIGN", "entity_ids": "c1,c2", "granularity": "TOTAL",
			"placement": "ALL_ON_TWITTER", "metric_groups": "ENGAGEMENT,BILLING",
			"start_time": "2026-09-29T07:00:00Z", "end_time": "2026-10-06T07:00:00Z",
		} {
			if got := q.Get(k); got != v {
				t.Errorf("POST %d %s = %q, want %q", i, k, got, v)
			}
		}
	}
	if ai.Currency != "EUR" || ai.Window != WindowLast7Days {
		t.Errorf("currency/window = %q/%q", ai.Currency, ai.Window)
	}
	want := []AudienceBucket{
		{Dimension: "age", Value: "25-34", Impressions: 400, Clicks: 20, CostMicros: 4_000_000},
		{Dimension: "age", Value: "18-24", Impressions: 200, Clicks: 10, CostMicros: 2_000_000},
		{Dimension: "gender", Value: "Female", Impressions: 400, Clicks: 20, CostMicros: 4_000_000},
		{Dimension: "gender", Value: "Male", Impressions: 200, Clicks: 10, CostMicros: 2_000_000},
		{Dimension: "platform", Value: "Desktop and laptop computers", Impressions: 400, Clicks: 20, CostMicros: 4_000_000},
		{Dimension: "platform", Value: "iOS", Impressions: 200, Clicks: 10, CostMicros: 2_000_000},
	}
	if len(ai.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
	for i, w := range want {
		w.Ctr = float64(w.Clicks) / float64(w.Impressions)
		if ai.Buckets[i] != w {
			t.Errorf("bucket %d = %+v, want %+v", i, ai.Buckets[i], w)
		}
	}
}

// More than 20 campaigns are batched (X's per-job entity cap) and the scope is deduplicated;
// more than MaxAudienceCampaigns is refused before any request.
func TestGetAudienceInsights_BatchingAndBound(t *testing.T) {
	f := newFakeXAudience(t)
	ids := append(audienceIDs(25), "c3", "c7") // duplicates collapse
	ai, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, ids)
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	posts, _, _ := f.snapshot()
	if len(posts) != 6 {
		t.Fatalf("%d job POSTs, want 6 (two batches x three segmentations)", len(posts))
	}
	seen := map[string]map[string]bool{}
	for _, q := range posts {
		batch := strings.Split(q.Get("entity_ids"), ",")
		if len(batch) > statsJobMaxEntities {
			t.Errorf("a job carries %d entity ids, over X's %d cap", len(batch), statsJobMaxEntities)
		}
		seg := q.Get("segmentation_type")
		if seen[seg] == nil {
			seen[seg] = map[string]bool{}
		}
		for _, id := range batch {
			if seen[seg][id] {
				t.Errorf("%s: campaign %s requested twice", seg, id)
			}
			seen[seg][id] = true
		}
	}
	for seg, got := range seen {
		if len(got) != 25 {
			t.Errorf("%s covered %d campaigns, want 25", seg, len(got))
		}
	}
	if ai.Buckets[0].Impressions != 25*200 {
		t.Errorf("top bucket = %+v, want every batch summed", ai.Buckets[0])
	}

	g := newFakeXAudience(t)
	_, err = g.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, audienceIDs(MaxAudienceCampaigns+1))
	if !errors.Is(err, ErrAudienceScopeTooLarge) {
		t.Errorf("err = %v, want ErrAudienceScopeTooLarge", err)
	}
	for name, scope := range map[string][]string{"empty": nil, "invalid": {"c1", "c/2"}, "padded": {" c1"}} {
		if _, err := g.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, scope); !errors.Is(err, ErrAudienceScopeInvalid) {
			t.Errorf("%s: err = %v, want ErrAudienceScopeInvalid", name, err)
		}
	}
	if posts, reads, gets := g.snapshot(); len(posts)+reads+gets != 0 {
		t.Errorf("refused scopes reached X: %d posts, %d status reads, %d account reads", len(posts), reads, gets)
	}
}

// The window is the metrics read's days on the ACCOUNT's calendar, as whole-hour UTC instants.
func TestGetAudienceInsights_WindowInAccountTimezone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tz         string
		now        time.Time
		window     MetricsWindow
		start, end string
	}{
		{"last 7 days, LA", "America/Los_Angeles", audienceNow, WindowLast7Days, "2026-09-29T07:00:00Z", "2026-10-06T07:00:00Z"},
		{"default is last 7 days", "America/Los_Angeles", audienceNow, "", "2026-09-29T07:00:00Z", "2026-10-06T07:00:00Z"},
		{"today, LA", "America/Los_Angeles", audienceNow, WindowToday, "2026-10-05T07:00:00Z", "2026-10-06T07:00:00Z"},
		{"yesterday, LA", "America/Los_Angeles", audienceNow, WindowYesterday, "2026-10-04T07:00:00Z", "2026-10-05T07:00:00Z"},
		// 03:00 UTC on Oct 6 is still Oct 5 in LA: the account's day, not UTC's.
		{"account day differs from UTC day", "America/Los_Angeles", time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC), WindowToday, "2026-10-05T07:00:00Z", "2026-10-06T07:00:00Z"},
		{"today, Tokyo", "Asia/Tokyo", audienceNow, WindowToday, "2026-10-04T15:00:00Z", "2026-10-05T15:00:00Z"},
		// LA falls back on 2026-11-01: the 7-day window spans 7 days and one hour of wall time.
		{"across DST fall-back", "America/Los_Angeles", time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC), WindowLast7Days, "2026-10-28T07:00:00Z", "2026-11-04T08:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeXAudience(t)
			f.set(func(f *fakeXAudience) {
				f.account = fmt.Sprintf(`{"data":{"id":"account123","timezone":%q}}`, tc.tz)
			})
			ai, err := f.client(tc.now).GetAudienceInsights(context.Background(), tc.window, []string{"c1"})
			if err != nil {
				t.Fatalf("GetAudienceInsights: %v", err)
			}
			posts, _, _ := f.snapshot()
			for _, q := range posts {
				if q.Get("start_time") != tc.start || q.Get("end_time") != tc.end {
					t.Errorf("window = [%s, %s), want [%s, %s)", q.Get("start_time"), q.Get("end_time"), tc.start, tc.end)
				}
			}
			if ai.Currency != "" {
				t.Errorf("currency = %q, want absent when the account carries none", ai.Currency)
			}
		})
	}
}

// A fractional-offset zone cannot be queried on its own days; a longer window is the metrics
// read's refusal. Neither creates a job.
func TestGetAudienceInsights_WindowRefusals(t *testing.T) {
	f := newFakeXAudience(t)
	f.set(func(f *fakeXAudience) { f.account = `{"data":{"id":"account123","timezone":"Asia/Kolkata"}}` })
	if _, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"}); !errors.Is(err, ErrReportWindowNotWholeHours) {
		t.Errorf("Kolkata: err = %v, want ErrReportWindowNotWholeHours", err)
	}
	if _, err := f.client(audienceNow).GetAudienceInsights(context.Background(), "LAST_30_DAYS", []string{"c1"}); !errors.Is(err, ErrUnsupportedWindow) {
		t.Errorf("30 days: err = %v, want ErrUnsupportedWindow", err)
	}
	if posts, _, _ := f.snapshot(); len(posts) != 0 {
		t.Errorf("%d jobs created for refused windows", len(posts))
	}
}

// Jobs that are still building are polled again; jobs that never finish fail the read, bounded
// either by the poll cap or by the context's deadline.
func TestGetAudienceInsights_Polling(t *testing.T) {
	f := newFakeXAudience(t)
	f.set(func(f *fakeXAudience) { f.pendingReads = 2 })
	if _, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"}); err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if _, reads, _ := f.snapshot(); reads != 3 {
		t.Errorf("%d status reads, want 3 (two pending, one finished)", reads)
	}

	g := newFakeXAudience(t)
	g.set(func(f *fakeXAudience) { f.pendingReads = -1 })
	_, err := g.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
	if !errors.Is(err, ErrAudienceJobsUnfinished) {
		t.Errorf("never finished: err = %v, want ErrAudienceJobsUnfinished", err)
	}
	if _, reads, _ := g.snapshot(); reads != audienceMaxPolls {
		t.Errorf("%d status reads, want the %d-read bound", reads, audienceMaxPolls)
	}

	h := newFakeXAudience(t)
	h.set(func(f *fakeXAudience) { f.pendingReads = -1 })
	c := h.client(audienceNow)
	c.audiencePollInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.GetAudienceInsights(ctx, WindowToday, []string{"c1"}); !errors.Is(err, ErrAudienceJobsUnfinished) {
		t.Errorf("deadline: err = %v, want ErrAudienceJobsUnfinished", err)
	}
}

// Every response that could carry two values for one key — at the envelope, inside an object, or
// under the decoder's case folding — is refused before it is decoded.
func TestGetAudienceInsights_DuplicateKeysAtAnyLevel(t *testing.T) {
	cases := map[string]func(*fakeXAudience){
		"account envelope": func(f *fakeXAudience) {
			f.account = `{"data":{"id":"account123","timezone":"UTC"},"Data":{"id":"account123","timezone":"UTC"}}`
		},
		"account currency case-folded": func(f *fakeXAudience) {
			f.account = `{"data":{"id":"account123","timezone":"UTC","currency":"USD","Currency":"EUR"}}`
		},
		"job create": func(f *fakeXAudience) {
			f.createBody = `{"data":{"id_str":"%s","ID_STR":"9"}}`
		},
		"job status": func(f *fakeXAudience) {
			f.statusBody = func(ids []string) string {
				return fmt.Sprintf(`{"data":[{"id_str":%q,"status":"PROCESSING","Status":"SUCCESS"}]}`, ids[0])
			}
		},
		"file entity id (KELVIN SIGN fold)": func(f *fakeXAudience) {
			f.file = func(string, []string) string {
				return `{"data":[{"id":"c1","K":1,"k":2,"id_data":[]}]}`
			}
		},
		"file entity id case-folded": func(f *fakeXAudience) {
			f.file = func(string, []string) string { return `{"data":[{"ID":"cX","id":"c1","id_data":[]}]}` }
		},
		"file segment name": func(f *fakeXAudience) {
			f.file = func(string, []string) string {
				return `{"data":[{"id":"c1","id_data":[{"segment":{"segment_name":"Male","Segment_Name":"Female"},"metrics":{}}]}]}`
			}
		},
		"file metrics": func(f *fakeXAudience) {
			f.file = func(string, []string) string {
				return `{"data":[{"id":"c1","id_data":[{"segment":{"segment_name":"Male"},"metrics":{"impressions":[1],"impressions":[999]}}]}]}`
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeXAudience(t)
			f.set(setup)
			_, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
			if err == nil || !strings.Contains(err.Error(), "cannot be trusted") {
				t.Errorf("err = %v, want an identityjson refusal", err)
			}
		})
	}
}

// Absent and null counters (the metric, or its single bucket) are X's "no activity" and read 0;
// the read still succeeds.
func TestGetAudienceInsights_NullCounters(t *testing.T) {
	f := newFakeXAudience(t)
	f.set(func(f *fakeXAudience) {
		f.file = func(string, []string) string {
			return `{"data":[{"id":"c1","id_data":[
				{"segment":{"segment_name":"Male"},"metrics":{"impressions":null,"clicks":[null]}},
				{"segment":{"segment_name":"Female"},"metrics":{"impressions":[10],"clicks":[1],"billed_charge_local_micro":null}}
			]}]}`
		}
	})
	ai, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	got := map[string]AudienceBucket{}
	for _, b := range ai.Buckets {
		if b.Dimension == "gender" {
			got[b.Value] = b
		}
	}
	if m := got["Male"]; m.Impressions != 0 || m.Clicks != 0 || m.CostMicros != 0 || m.Ctr != 0 {
		t.Errorf("Male = %+v, want zeros", m)
	}
	if fe := got["Female"]; fe.Impressions != 10 || fe.Clicks != 1 || fe.CostMicros != 0 || fe.Ctr != 0.1 {
		t.Errorf("Female = %+v", fe)
	}
}

// Every malformed or untrustworthy file fails the whole read, and no error carries upstream text.
func TestGetAudienceInsights_MalformedFilesFailWhole(t *testing.T) {
	row := func(seg, metrics string) string {
		return `{"data":[{"id":"c1","id_data":[{"segment":` + seg + `,"metrics":` + metrics + `}]}]}`
	}
	for name, body := range map[string]string{
		"not json":                 `{"data":[`,
		"no data field":            `{"request":{}}`,
		"null data":                `{"data":null}`,
		"foreign entity":           `{"data":[{"id":"` + audienceCanary + `","id_data":[]}]}`,
		"another account's id":     `{"data":[{"id":"c9","id_data":[]}]}`,
		"entity without id":        `{"data":[{"id_data":[]}]}`,
		"entity repeated":          `{"data":[{"id":"c1","id_data":[]},{"id":"c1","id_data":[]}]}`,
		"id_data absent":           `{"data":[{"id":"c1"}]}`,
		"id_data null":             `{"data":[{"id":"c1","id_data":null}]}`,
		"segment null":             row(`null`, `{}`),
		"segment name absent":      row(`{"segment_value":"x"}`, `{}`),
		"segment name unsafe":      row(`{"segment_name":"`+audienceCanary+`"}`, `{}`),
		"segment name edge space":  row(`{"segment_name":"Male "}`, `{}`),
		"segment repeated":         `{"data":[{"id":"c1","id_data":[{"segment":{"segment_name":"Male"},"metrics":{}},{"segment":{"segment_name":"Male"},"metrics":{}}]}]}`,
		"metrics absent":           `{"data":[{"id":"c1","id_data":[{"segment":{"segment_name":"Male"}}]}]}`,
		"counter is a string":      row(`{"segment_name":"Male"}`, `{"impressions":["12"]}`),
		"counter is a fraction":    row(`{"segment_name":"Male"}`, `{"clicks":[1.5]}`),
		"counter is negative":      row(`{"segment_name":"Male"}`, `{"billed_charge_local_micro":[-1]}`),
		"counter has two buckets":  row(`{"segment_name":"Male"}`, `{"impressions":[1,2]}`),
		"counter is empty":         row(`{"segment_name":"Male"}`, `{"impressions":[]}`),
		"counter is a scalar":      row(`{"segment_name":"Male"}`, `{"impressions":5}`),
		"counter overflows int64":  row(`{"segment_name":"Male"}`, `{"impressions":[9223372036854775808]}`),
		"malformed utf-8 (silent)": "{\"data\":[{\"id\":\"c1\",\"id_data\":[{\"segment\":{\"segment_name\":\"M\xffale\"},\"metrics\":{}}]}]}",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeXAudience(t)
			f.set(func(f *fakeXAudience) { f.file = func(string, []string) string { return body } })
			ai, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
			if err == nil {
				t.Fatalf("read succeeded with %+v", ai)
			}
			if strings.Contains(err.Error(), "CANARY") {
				t.Errorf("error echoes upstream text: %v", err)
			}
		})
	}
}

// Summing across campaigns must not wrap.
func TestGetAudienceInsights_SumOverflowFails(t *testing.T) {
	f := newFakeXAudience(t)
	f.set(func(f *fakeXAudience) {
		f.file = func(_ string, ids []string) string {
			ents := make([]string, 0, len(ids))
			for _, id := range ids {
				ents = append(ents, `{"id":"`+id+`","id_data":[{"segment":{"segment_name":"Male"},"metrics":{"impressions":[9223372036854775807]}}]}`)
			}
			return `{"data":[` + strings.Join(ents, ",") + `]}`
		}
	})
	if _, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1", "c2"}); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Errorf("err = %v, want an overflow refusal", err)
	}
}

// Upstream statuses fail the read with a typed error carrying no upstream body text.
func TestGetAudienceInsights_UpstreamStatuses(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run("job create "+strconv.Itoa(status), func(t *testing.T) {
			f := newFakeXAudience(t)
			f.set(func(f *fakeXAudience) { f.createStatus = status })
			_, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
			var ae *apiError
			if !errors.As(err, &ae) || ae.StatusCode != status {
				t.Fatalf("err = %v, want an apiError %d", err, status)
			}
			if strings.Contains(err.Error(), "CANARY") {
				t.Errorf("error echoes upstream text: %v", err)
			}
			// A create is not retried, even on 429: X may have built the job.
			if posts, reads, _ := f.snapshot(); len(posts) != 1 || reads != 0 {
				t.Errorf("%d posts and %d status reads after a refused create, want 1 and 0", len(posts), reads)
			}
		})
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run("account read "+strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, audienceCanary)
			}))
			t.Cleanup(srv.Close)
			c := NewClient(Credentials{ConsumerKey: "k", ConsumerSecret: "s", AccessToken: "t", AccessTokenSecret: "ts"},
				AccountConfig{AccountID: "account123"}, WithBaseURL(srv.URL), WithWriteDelay(0), WithClock(func() time.Time { return audienceNow }))
			_, err := c.GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
			var ae *apiError
			if !errors.As(err, &ae) || ae.StatusCode != status || strings.Contains(err.Error(), "CANARY") {
				t.Errorf("err = %v, want a body-free apiError %d", err, status)
			}
		})
	}
	t.Run("status read 429 past the retry cap", func(t *testing.T) {
		f := newFakeXAudience(t)
		// The status read answers 429 with a reset an hour away, which the client refuses to
		// sleep through (maxRetryWait), so the read fails at once rather than backing off.
		reset := strconv.FormatInt(audienceNow.Add(time.Hour).Unix(), 10)
		f.set(func(f *fakeXAudience) {
			f.statusReject = func(w http.ResponseWriter) {
				w.Header().Set("X-Rate-Limit-Reset", reset)
				w.WriteHeader(http.StatusTooManyRequests)
			}
		})
		_, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
			t.Errorf("err = %v, want an apiError 429", err)
		}
	})
}

// Job lifecycle defects fail the read.
func TestGetAudienceInsights_JobDefects(t *testing.T) {
	status := func(body string) func(*fakeXAudience) {
		return func(f *fakeXAudience) {
			f.statusBody = func(ids []string) string { return strings.ReplaceAll(body, "ID", ids[0]) }
		}
	}
	for name, setup := range map[string]func(*fakeXAudience){
		"failed job":           status(`{"data":[{"id_str":"ID","status":"FAILED"}]}`),
		"cancelled job":        status(`{"data":[{"id_str":"ID","status":"CANCELLED"}]}`),
		"success without url":  status(`{"data":[{"id_str":"ID","status":"SUCCESS","url":null}]}`),
		"unknown status":       status(`{"data":[{"id_str":"ID","status":"` + audienceCanary + `"}]}`),
		"unasked job":          status(`{"data":[{"id_str":"424242","status":"PROCESSING"}]}`),
		"job named twice":      status(`{"data":[{"id_str":"ID","status":"PROCESSING"},{"id_str":"ID","status":"PROCESSING"}]}`),
		"no data":              status(`{}`),
		"data not a list":      status(`{"data":{"id_str":"ID"}}`),
		"file on foreign host": status(`{"data":[{"id_str":"ID","status":"SUCCESS","url":"https://evil.example/f.json.gz"}]}`),
		"create without id":    func(f *fakeXAudience) { f.createBody = `{"data":{"status":"PROCESSING","x":"%s"}}` },
		"create bad id":        func(f *fakeXAudience) { f.createBody = `{"data":{"id_str":"12a%s"}}` },
		"create no data":       func(f *fakeXAudience) { f.createBody = `{"x":"%s"}` },
		"account other id":     func(f *fakeXAudience) { f.account = `{"data":{"id":"other","timezone":"UTC"}}` },
		"account no timezone":  func(f *fakeXAudience) { f.account = `{"data":{"id":"account123"}}` },
		"account bad zone":     func(f *fakeXAudience) { f.account = `{"data":{"id":"account123","timezone":"` + audienceCanary + `"}}` },
		"account bad currency": func(f *fakeXAudience) {
			f.account = `{"data":{"id":"account123","timezone":"UTC","currency":"` + audienceCanary + `"}}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeXAudience(t)
			f.set(setup)
			_, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, []string{"c1"})
			if err == nil {
				t.Fatal("read succeeded")
			}
			if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "evil") {
				t.Errorf("error echoes upstream text: %v", err)
			}
		})
	}
}

// A job's file may only describe that job's campaigns: an id from the OTHER batch is foreign to it.
func TestGetAudienceInsights_EntityMustBeInItsJobsBatch(t *testing.T) {
	f := newFakeXAudience(t)
	f.set(func(f *fakeXAudience) {
		f.file = func(seg string, ids []string) string {
			if ids[0] == "c20" {
				return defaultAudienceFile(seg, []string{"c0"}) // in scope, but not this job's
			}
			return defaultAudienceFile(seg, ids)
		}
	})
	if _, err := f.client(audienceNow).GetAudienceInsights(context.Background(), WindowToday, audienceIDs(21)); err == nil || !strings.Contains(err.Error(), "outside this job's scope") {
		t.Errorf("err = %v, want a foreign-entity refusal", err)
	}
}

// The job budget check runs before any job is created.
func TestGetAudienceInsights_BudgetCheckedBeforeJobs(t *testing.T) {
	f := newFakeXAudience(t)
	c := f.client(audienceNow)
	c.writeDelay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.GetAudienceInsights(ctx, WindowToday, audienceIDs(21)); !errors.Is(err, ErrStatsJobBudget) {
		t.Errorf("err = %v, want ErrStatsJobBudget", err)
	}
	if posts, _, _ := f.snapshot(); len(posts) != 0 {
		t.Errorf("%d jobs created on a budget that could not fit them", len(posts))
	}
}

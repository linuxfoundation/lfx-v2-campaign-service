// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// audienceServer serves the two audience breakdown reads. pages maps the `breakdowns` query
// value to that breakdown's canned page bodies, served one per request in order. Every request
// URI is recorded under recordedURIs' lock, and the page index is taken under the same lock,
// so the handler goroutines and the test body never race.
func audienceServer(t *testing.T, pages map[string][]string) (*httptest.Server, *recordedURIs) {
	t.Helper()
	rec := &recordedURIs{}
	served := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.uris = append(rec.uris, r.URL.RequestURI())
		b := r.URL.Query().Get("breakdowns")
		n := served[b]
		served[b] = n + 1
		rec.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer tok-secret-abc" {
			t.Errorf("Authorization = %q", got)
		}
		canned, ok := pages[b]
		if !ok || n >= len(canned) {
			t.Errorf("unexpected request for breakdowns=%q page %d", b, n+1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canned[n]))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newAudienceClient(srv *httptest.Server, opts ...Option) *Client {
	all := append([]Option{WithBaseURL(srv.URL), WithClock(fixedMetaClock())}, opts...)
	return NewClient(Credentials{AccessToken: "tok-secret-abc"}, AccountConfig{}, all...)
}

const (
	ageGenderKey = "age,gender"
	placementKey = "publisher_platform,platform_position"
)

func ageRow(campaign, age, gender string, impressions, clicks int, spend string) string {
	return fmt.Sprintf(`{"campaign_id":%q,"age":%q,"gender":%q,"impressions":"%d","clicks":"%d","spend":%q,"account_currency":"EUR","date_start":"2026-07-01","date_stop":"2026-07-14"}`,
		campaign, age, gender, impressions, clicks, spend)
}

func placementRow(campaign, platform, position string, impressions, clicks int, spend string) string {
	return fmt.Sprintf(`{"campaign_id":%q,"publisher_platform":%q,"platform_position":%q,"impressions":"%d","clicks":"%d","spend":%q,"account_currency":"EUR"}`,
		campaign, platform, position, impressions, clicks, spend)
}

func audiencePage(next string, rows ...string) string {
	paging := `{}`
	if next != "" {
		paging = fmt.Sprintf(`{"cursors":{"after":%q},"next":"https://graph.facebook.com/v21.0/act_777/insights?access_token=SHOULD-NOT-BE-FOLLOWED&after=%s"}`, next, next)
	}
	return `{"data":[` + strings.Join(rows, ",") + `],"paging":` + paging + `}`
}

func emptyPlacement() map[string][]string {
	return map[string][]string{placementKey: {audiencePage("")}}
}

func withPages(base map[string][]string, key string, pages ...string) map[string][]string {
	out := map[string][]string{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = pages
	return out
}

func TestGetAudienceInsights_ParsesAndAggregatesBothBreakdowns(t *testing.T) {
	srv, rec := audienceServer(t, map[string][]string{
		ageGenderKey: {audiencePage("",
			ageRow("111", "25-34", "female", 1000, 50, "10.50"),
			ageRow("222", "25-34", "female", 3000, 10, "4.25"),
			ageRow("111", "65+", "unknown", 200, 2, ""),
		)},
		placementKey: {audiencePage("",
			placementRow("111", "instagram", "instagram_stories", 800, 8, "3"),
			placementRow("222", "facebook", "feed", 900, 90, "7.10"),
		)},
	})
	c := newAudienceClient(srv)

	ai, err := c.GetAudienceInsights(context.Background(), "act_777", WindowLast7Days, []string{"111", "222"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if ai.Window != WindowLast7Days || ai.Currency != "EUR" {
		t.Errorf("window/currency = %q/%q", ai.Window, ai.Currency)
	}
	want := []AudienceBucket{
		// 25-34/female summed across BOTH campaigns; CTR computed after the sum, not averaged.
		{Dimension: AudienceDimensionAgeGender, Age: "25-34", Gender: "female", Impressions: 4000, Clicks: 60, CostMicros: 14_750_000, Ctr: 60.0 / 4000},
		{Dimension: AudienceDimensionAgeGender, Age: "65+", Gender: "unknown", Impressions: 200, Clicks: 2, CostMicros: 0, Ctr: 0.01},
		{Dimension: AudienceDimensionPlacement, PublisherPlatform: "facebook", PlatformPosition: "feed", Impressions: 900, Clicks: 90, CostMicros: 7_100_000, Ctr: 0.1},
		{Dimension: AudienceDimensionPlacement, PublisherPlatform: "instagram", PlatformPosition: "instagram_stories", Impressions: 800, Clicks: 8, CostMicros: 3_000_000, Ctr: 0.01},
	}
	if len(ai.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
	for i := range want {
		if ai.Buckets[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, ai.Buckets[i], want[i])
		}
	}

	uris := rec.all()
	if len(uris) != 2 {
		t.Fatalf("requests = %d, want one per breakdown: %v", len(uris), uris)
	}
	for _, uri := range uris {
		u, perr := url.Parse(uri)
		if perr != nil {
			t.Fatal(perr)
		}
		q := u.Query()
		if u.Path != "/act_777/insights" {
			t.Errorf("path = %q", u.Path)
		}
		if q.Get("level") != "campaign" || q.Get("date_preset") != "last_7d" || q.Get("limit") != "500" {
			t.Errorf("query = %v", q)
		}
		if q.Get("fields") != "campaign_id,impressions,clicks,spend,account_currency" {
			t.Errorf("fields = %q", q.Get("fields"))
		}
		if q.Has("access_token") || strings.Contains(uri, "tok-secret-abc") {
			t.Errorf("the token must travel in the header only: %s", uri)
		}
		// The filter is the tenant boundary: decode it and compare structurally.
		var filter []struct {
			Field    string   `json:"field"`
			Operator string   `json:"operator"`
			Value    []string `json:"value"`
		}
		if jerr := json.Unmarshal([]byte(q.Get("filtering")), &filter); jerr != nil {
			t.Fatalf("filtering %q: %v", q.Get("filtering"), jerr)
		}
		if len(filter) != 1 || filter[0].Field != "campaign.id" || filter[0].Operator != "IN" ||
			strings.Join(filter[0].Value, ",") != "111,222" {
			t.Errorf("filtering = %+v, want campaign.id IN [111 222]", filter)
		}
	}
	if b0, b1 := mustQuery(t, uris[0]).Get("breakdowns"), mustQuery(t, uris[1]).Get("breakdowns"); b0 != ageGenderKey || b1 != placementKey {
		t.Errorf("breakdowns = %q, %q", b0, b1)
	}
}

func mustQuery(t *testing.T, uri string) url.Values {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// The filter must name the project's OWN ids only, deduplicated — never widened, never empty.
func TestGetAudienceInsights_FilterCarriesOnlyTheScope(t *testing.T) {
	srv, rec := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, audiencePage("")))
	c := newAudienceClient(srv)
	if _, err := c.GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"333", "333", "444"}); err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	f := mustQuery(t, rec.all()[0]).Get("filtering")
	if f != `[{"field":"campaign.id","operator":"IN","value":["333","444"]}]` {
		t.Errorf("filtering = %s", f)
	}
}

func TestGetAudienceInsights_ScopeRefusedBeforeAnyRequest(t *testing.T) {
	tooMany := make([]string, MaxAudienceCampaigns+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(1000 + i)
	}
	for _, tc := range []struct {
		name    string
		account string
		ids     []string
		want    error
	}{
		{"empty scope never widens to the account", "act_777", nil, ErrAudienceScopeInvalid},
		{"non-numeric id", "act_777", []string{"111", "12a"}, ErrAudienceScopeInvalid},
		{"leading zero", "act_777", []string{"0111"}, ErrAudienceScopeInvalid},
		{"too many campaigns", "act_777", tooMany, ErrAudienceScopeTooLarge},
		{"bare-digit account", "777", []string{"111"}, ErrInvalidAccountID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := audienceServer(t, nil)
			c := newAudienceClient(srv)
			_, err := c.GetAudienceInsights(context.Background(), tc.account, WindowLast30Days, tc.ids)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if n := rec.count(); n != 0 {
				t.Errorf("Meta was contacted %d time(s) for a refused scope", n)
			}
		})
	}
}

func TestGetAudienceInsights_WindowMapsToDatePreset(t *testing.T) {
	for window, preset := range datePresetFor {
		t.Run(string(window), func(t *testing.T) {
			srv, rec := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, audiencePage("")))
			ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", window, []string{"111"})
			if err != nil {
				t.Fatalf("GetAudienceInsights: %v", err)
			}
			if ai.Window != window {
				t.Errorf("window = %q", ai.Window)
			}
			for _, uri := range rec.all() {
				if got := mustQuery(t, uri).Get("date_preset"); got != preset {
					t.Errorf("date_preset = %q, want %q", got, preset)
				}
			}
		})
	}
	t.Run("empty defaults to last_30d", func(t *testing.T) {
		srv, rec := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, audiencePage("")))
		if _, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", "", []string{"111"}); err != nil {
			t.Fatal(err)
		}
		if got := mustQuery(t, rec.all()[0]).Get("date_preset"); got != "last_30d" {
			t.Errorf("date_preset = %q", got)
		}
	})
	t.Run("unsupported window never reaches Meta", func(t *testing.T) {
		srv, rec := audienceServer(t, nil)
		if _, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", "LAST_YEAR", []string{"111"}); err == nil {
			t.Fatal("expected an error")
		}
		if rec.count() != 0 {
			t.Error("an unsupported window reached Meta")
		}
	})
}

func TestGetAudienceInsights_FollowsCursorAcrossPages(t *testing.T) {
	srv, rec := audienceServer(t, withPages(emptyPlacement(), ageGenderKey,
		audiencePage("cur-1", ageRow("111", "18-24", "male", 10, 1, "1")),
		audiencePage("", ageRow("111", "18-24", "female", 30, 3, "2")),
	))
	ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if len(ai.Buckets) != 2 || ai.Buckets[0].Gender != "female" || ai.Buckets[1].Gender != "male" {
		t.Errorf("buckets = %+v", ai.Buckets)
	}
	uris := rec.all()
	if len(uris) != 3 {
		t.Fatalf("requests = %d, want 2 pages + 1 placement read", len(uris))
	}
	q := mustQuery(t, uris[1])
	if q.Get("after") != "cur-1" || q.Has("access_token") {
		t.Errorf("second page must rebuild the path from the cursor, not follow paging.next: %s", uris[1])
	}
}

// The walk is bounded: a next link on the LAST allowed page is the read-plus-one signal that
// more rows exist, and the read fails rather than presenting a truncated distribution.
func TestGetAudienceInsights_PageBoundFailsClosed(t *testing.T) {
	pages := make([]string, audienceMaxPages)
	for i := range pages {
		pages[i] = audiencePage(fmt.Sprintf("cur-%d", i), ageRow("111", strconv.Itoa(18+i), "male", 1, 0, ""))
	}
	srv, rec := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, pages...))
	ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("error = %v, want the page bound to fail the read", err)
	}
	if ai != nil {
		t.Errorf("a bounded-out read must return no rows, got %+v", ai)
	}
	if n := rec.count(); n != audienceMaxPages {
		t.Errorf("requests = %d, want exactly %d (the bound)", n, audienceMaxPages)
	}
}

func TestGetAudienceInsights_PagingDefectsFail(t *testing.T) {
	noCursor := `{"data":[],"paging":{"next":"https://graph.facebook.com/x"}}`
	repeated := audiencePage("same")
	for name, pages := range map[string][]string{
		"next without cursor": {noCursor},
		"repeated cursor":     {repeated, repeated},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, pages...))
			if _, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// Unknown breakdown values are kept verbatim when they match the safe charset, and refuse the
// whole read when they do not — never echoed into the error.
func TestGetAudienceInsights_BreakdownValueTrust(t *testing.T) {
	t.Run("unknown but safe value is kept verbatim", func(t *testing.T) {
		srv, _ := audienceServer(t, map[string][]string{
			ageGenderKey: {audiencePage("", ageRow("111", "Unknown", "unknown", 5, 0, ""))},
			placementKey: {audiencePage("", placementRow("111", "threads", "threads_feed", 7, 1, "0.01"))},
		})
		ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
		if err != nil {
			t.Fatalf("GetAudienceInsights: %v", err)
		}
		if ai.Buckets[0].Age != "Unknown" || ai.Buckets[1].PublisherPlatform != "threads" || ai.Buckets[1].PlatformPosition != "threads_feed" {
			t.Errorf("buckets = %+v", ai.Buckets)
		}
	})
	for name, row := range map[string]string{
		"markup":          ageRow("111", "<script>leak-me</script>", "male", 1, 0, ""),
		"whitespace":      ageRow("111", "25 34", "male", 1, 0, ""),
		"empty":           ageRow("111", "", "male", 1, 0, ""),
		"too long":        ageRow("111", strings.Repeat("a", 65), "male", 1, 0, ""),
		"missing key":     `{"campaign_id":"111","age":"25-34","impressions":"1","clicks":"0","spend":"","account_currency":"EUR"}`,
		"non-string":      `{"campaign_id":"111","age":25,"gender":"male","impressions":"1","clicks":"0","spend":"","account_currency":"EUR"}`,
		"null breakdown":  `{"campaign_id":"111","age":null,"gender":"male","impressions":"1","clicks":"0","spend":"","account_currency":"EUR"}`,
		"control charset": ageRow("111", "25-34\u0000", "male", 1, 0, ""),
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, audiencePage("", row)))
			ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
			if err == nil {
				t.Fatalf("expected the untrustworthy row to fail the read, got %+v", ai)
			}
			if strings.Contains(err.Error(), "leak-me") {
				t.Errorf("error echoes upstream content: %v", err)
			}
		})
	}
}

func TestGetAudienceInsights_MalformedRowsFailTheWholeRead(t *testing.T) {
	for name, page := range map[string]string{
		"no data field":              `{"paging":{}}`,
		"data null":                  `{"data":null}`,
		"row not an object":          audiencePage("", `"oops"`),
		"campaign outside scope":     audiencePage("", ageRow("999", "25-34", "male", 1, 0, "")),
		"missing campaign id":        audiencePage("", `{"age":"25-34","gender":"male","impressions":"1","clicks":"0","spend":"","account_currency":"EUR"}`),
		"non-numeric impressions":    audiencePage("", strings.Replace(ageRow("111", "25-34", "male", 1, 0, ""), `"impressions":"1"`, `"impressions":"lots"`, 1)),
		"negative clicks":            audiencePage("", strings.Replace(ageRow("111", "25-34", "male", 1, 0, ""), `"clicks":"0"`, `"clicks":"-4"`, 1)),
		"malformed spend":            audiencePage("", ageRow("111", "25-34", "male", 1, 0, "12,50")),
		"negative spend":             audiencePage("", ageRow("111", "25-34", "male", 1, 0, "-1")),
		"bad currency":               audiencePage("", strings.Replace(ageRow("111", "25-34", "male", 1, 0, ""), `"EUR"`, `"eu"`, 1)),
		"missing currency":           audiencePage("", strings.Replace(ageRow("111", "25-34", "male", 1, 0, ""), `,"account_currency":"EUR"`, ``, 1)),
		"mixed currencies":           audiencePage("", ageRow("111", "25-34", "male", 1, 0, ""), strings.Replace(ageRow("111", "35-44", "male", 1, 0, ""), `"EUR"`, `"USD"`, 1)),
		"duplicate json key":         audiencePage("", `{"campaign_id":"111","campaign_id":"999","age":"25-34","gender":"male","impressions":"1","clicks":"0","spend":"","account_currency":"EUR"}`),
		"duplicate campaign+segment": audiencePage("", ageRow("111", "25-34", "male", 1, 0, ""), ageRow("111", "25-34", "male", 1, 0, "")),
		// Case-variant repeats: encoding/json would keep the LAST (in-scope, plausible) value.
		"case-variant campaign id": audiencePage("", `{"Campaign_ID":"999","campaign_id":"111","age":"25-34","gender":"male","impressions":"1","clicks":"0","spend":"","account_currency":"EUR"}`),
		"case-variant spend":       audiencePage("", `{"campaign_id":"111","age":"25-34","gender":"male","impressions":"1","clicks":"0","Spend":"999","spend":"1","account_currency":"EUR"}`),
		"long-s spend":             audiencePage("", "{\"campaign_id\":\"111\",\"age\":\"25-34\",\"gender\":\"male\",\"impressions\":\"1\",\"clicks\":\"0\",\"\u017fpend\":\"999\",\"spend\":\"1\",\"account_currency\":\"EUR\"}"),
		"truncated json":           `{"data":[{"campaign_id":"111"`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, page))
			ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
			if err == nil {
				t.Fatalf("expected an error, got %+v", ai)
			}
			if ai != nil {
				t.Errorf("a failed read must return no partial rows, got %+v", ai)
			}
		})
	}
}

// The duplicate-key guard is what stops `{"campaign_id":"<ours>",…,"campaign_id":"<theirs>"}`
// from passing the scope check on whichever value encoding/json happens to keep.
func TestRejectDuplicateKeys(t *testing.T) {
	for raw, wantErr := range map[string]bool{
		`{"a":"1","b":{"a":"nested ok"}}`: false,
		`{"a":"1","a":"2"}`:               true,
		`["a"]`:                           true,
		`{"a":`:                           true,
		// encoding/json matches keys case-insensitively and keeps the last, so a case-variant
		// repeat is as ambiguous as an exact one.
		`{"Campaign_ID":"999","campaign_id":"111"}`: true,
		`{"Spend":"9","spend":"1"}`:                 true,
		// KELVIN SIGN and LONG S fold onto k and s in the decoder, written raw and \u-escaped.
		"{\"\u212a\":\"1\",\"k\":\"2\"}":         true,
		"{\"\\u212a\":\"1\",\"k\":\"2\"}":        true,
		"{\"\u017fpend\":\"1\",\"spend\":\"2\"}": true,
	} {
		if err := rejectDuplicateKeys(json.RawMessage(raw)); (err != nil) != wantErr {
			t.Errorf("%s: err = %v, wantErr %v", raw, err, wantErr)
		}
	}
}

// One failing breakdown fails the read: age+gender without placement is a partial picture.
func TestGetAudienceInsights_UpstreamFailuresReturnNoRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, `{"error":{"message":"Invalid OAuth access token","type":"OAuthException","code":190}}`},
		{"403", http.StatusForbidden, `{"error":{"message":"no permission","type":"OAuthException","code":200}}`},
		{"400 invalid param", http.StatusBadRequest, `{"error":{"message":"bad breakdown","type":"OAuthException","code":100}}`},
		{"500", http.StatusInternalServerError, `{"error":{"message":"oops","code":1}}`},
		{"502 html", http.StatusBadGateway, `<html>bad gateway</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordedURIs{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.add(r.URL.RequestURI())
				if r.URL.Query().Get("breakdowns") == ageGenderKey {
					_, _ = w.Write([]byte(audiencePage("", ageRow("111", "25-34", "male", 1, 0, ""))))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
				t.Fatalf("error = %v, want an *APIError with status %d", err, tc.status)
			}
			if ai != nil {
				t.Errorf("the loaded breakdown must not be returned on its own: %+v", ai)
			}
		})
	}
}

func TestGetAudienceInsights_ThrottleRetriesThenFails(t *testing.T) {
	t.Run("429 after the retry budget is an error", func(t *testing.T) {
		rec := &recordedURIs{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.add(r.URL.RequestURI())
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":17}}`))
		}))
		t.Cleanup(srv.Close)
		c := newAudienceClient(srv, withRetryBaseDelay(time.Millisecond))
		_, err := c.GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("error = %v, want a 429 *APIError", err)
		}
		if n := rec.count(); n != retryMax+1 {
			t.Errorf("attempts = %d, want %d", n, retryMax+1)
		}
	})
	t.Run("a throttle that clears is retried to success", func(t *testing.T) {
		rec := &recordedURIs{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec.add(r.URL.RequestURI()) == 0 {
				w.WriteHeader(http.StatusBadRequest) // Meta's common throttle shape: 400 + rate-limit code
				_, _ = w.Write([]byte(`{"error":{"message":"too many calls","code":80004}}`))
				return
			}
			_, _ = w.Write([]byte(audiencePage("")))
		}))
		t.Cleanup(srv.Close)
		c := newAudienceClient(srv, withRetryBaseDelay(time.Millisecond))
		if _, err := c.GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"}); err != nil {
			t.Fatalf("GetAudienceInsights: %v", err)
		}
		if n := rec.count(); n != 3 {
			t.Errorf("requests = %d, want 1 throttled + 2 breakdowns", n)
		}
	})
}

// Meta's authoritative empty answer is an empty result with no currency, not an error.
func TestGetAudienceInsights_NoDeliveryIsEmpty(t *testing.T) {
	srv, _ := audienceServer(t, withPages(emptyPlacement(), ageGenderKey, audiencePage("")))
	ai, err := newAudienceClient(srv).GetAudienceInsights(context.Background(), "act_777", WindowLast30Days, []string{"111"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if ai.Buckets == nil || len(ai.Buckets) != 0 || ai.Currency != "" {
		t.Errorf("got %+v, want an empty non-nil bucket list and no currency", ai)
	}
}

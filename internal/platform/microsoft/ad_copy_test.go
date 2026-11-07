// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// adBodyCapture records the raw AddAds body; the handler runs on the server goroutine, so reads
// go through the mutex.
type adBodyCapture struct {
	mu   sync.Mutex
	body string
}

func (c *adBodyCapture) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

// captureAdBody returns a client whose AddAds POST body is recorded raw, every route served by
// the shared campaignsAPI script, and a counter of every API request.
func captureAdBody(t *testing.T, api *campaignsAPI) (*Client, *adBodyCapture, *atomic.Int32) {
	t.Helper()
	got := &adBodyCapture{}
	var calls atomic.Int32
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Ads") {
			b, _ := io.ReadAll(r.Body)
			got.mu.Lock()
			got.body = string(b)
			got.mu.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		base(w, r)
	})
	return c, got, &calls
}

// TestCreateCampaign_PathsSentOnResponsiveSearchAd pins the wire mapping: path1/path2 reach the
// AddAds body as the ResponsiveSearchAd's Path1/Path2, trimmed.
func TestCreateCampaign_PathsSentOnResponsiveSearchAd(t *testing.T) {
	c, got, _ := captureAdBody(t, &campaignsAPI{})
	in := validInput()
	in.Path1, in.Path2 = " kubecon ", "register"
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	for _, want := range []string{`"Path1":"kubecon"`, `"Path2":"register"`} {
		if !strings.Contains(got.get(), want) {
			t.Errorf("AddAds body lacks %s: %s", want, got.get())
		}
	}
}

// TestCreateCampaign_NoPathsSendsNoPathKeys keeps the pre-#563 body byte-for-byte on the path
// keys: neither is sent when neither is supplied.
func TestCreateCampaign_NoPathsSendsNoPathKeys(t *testing.T) {
	c, got, _ := captureAdBody(t, &campaignsAPI{})
	if _, err := c.CreateCampaign(context.Background(), validInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got.get() == "" {
		t.Fatal("AddAds body not captured")
	}
	if strings.Contains(got.get(), "Path1") || strings.Contains(got.get(), "Path2") {
		t.Errorf("AddAds body carries a path key with no paths supplied: %s", got.get())
	}
}

// TestCreateCampaign_PathRefusedBeforeAnyRequest: a bad path, or a display URL the paths push
// over the cap, is a clean (nil, PreflightRefusal) with no request sent.
func TestCreateCampaign_PathRefusedBeforeAnyRequest(t *testing.T) {
	cases := map[string]struct {
		url, p1, p2 string
		want        string
	}{
		"path2 without path1":  {p2: "register", want: "path2: requires path1"},
		"slash":                {p1: "a/b", want: "forward slash"},
		"too long":             {p1: "abcdefghijklmnop", want: "exceeding 15"},
		"wide too long":        {p1: "東京東京東京東京", want: "exceeding 7"},
		"blank":                {p1: "   ", want: "must not be blank"},
		"control":              {p1: "a\tb", want: "control character"},
		"no word":              {p1: "--", want: "letter or digit"},
		"display URL over cap": {url: "https://" + strings.Repeat("a", 40) + ".example.org/r", p1: "abcdefghijklmno", p2: "abcdefghijklmno", want: "display URL"},
		// 36-char host + "/" + 15 + "/" + 15 = 68: one over, counting both separators.
		"display URL 68": {url: "https://" + strings.Repeat("a", 24) + ".example.org/r", p1: "abcdefghijklmno", p2: "abcdefghijklmno", want: "is 68 characters"},
		// 50-char host + "/abcde" fits; path2 alone pushes it over.
		"path2 pushes over cap": {url: "https://" + strings.Repeat("a", 38) + ".example.org/r", p1: "abcde", p2: "abcdefghijklmno", want: "display URL"},
		// A wide path holds the whole display URL to 33: 35 + "/" + 2 = 38.
		"wide path display cap": {url: "https://" + strings.Repeat("a", 23) + ".example.org/r", p1: "東京", want: "exceeding the 33 limit"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, calls := captureAdBody(t, &campaignsAPI{})
			in := validInput()
			if tc.url != "" {
				in.RegistrationURL = tc.url
			}
			in.Path1, in.Path2 = tc.p1, tc.p2
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("CreateCampaign = (%v, %v); want (nil, refusal)", res, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
			var pr *domain.PreflightRefusal
			if !errors.As(err, &pr) {
				t.Errorf("error is not a PreflightRefusal: %v", err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d request(s) sent; a refusal must precede every request", n)
			}
		})
	}
}

// TestCreateCampaign_DisplayURLWithPathsAtCapIsAccepted is the boundary twin of the cap case: a
// host plus "/path1/path2" of exactly 67 characters is sent.
func TestCreateCampaign_DisplayURLWithPathsAtCapIsAccepted(t *testing.T) {
	// 35-char host + "/" + 15 + "/" + 15 = 67.
	host := strings.Repeat("a", 23) + ".example.org"
	if len(host) != 35 {
		t.Fatalf("fixture host is %d chars", len(host))
	}
	c, got, _ := captureAdBody(t, &campaignsAPI{})
	in := validInput()
	in.RegistrationURL = "https://" + host + "/r"
	in.Path1, in.Path2 = "abcdefghijklmno", "abcdefghijklmno"
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign at the 67-char cap: %v", err)
	}
	if !strings.Contains(got.get(), `"Path2":"abcdefghijklmno"`) {
		t.Errorf("AddAds body lacks Path2: %s", got.get())
	}
}

// TestCreateCampaign_ReusedAdWarnsCallerCopyNotApplied: a retry that finds the ad by its final
// URL keeps that ad's copy, and the steps say the caller's copy was not applied.
func TestCreateCampaign_ReusedAdWarnsCallerCopyNotApplied(t *testing.T) {
	in := validInput()
	in.Headlines = []string{"One headline", "Two headline", "Three headline"}
	finalURL := buildAdFinalURL(in)
	api := &campaignsAPI{adGetBody: `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !stepsContain(res.Steps, "caller ad copy NOT applied to reused ad 222") {
		t.Errorf("steps lack the reuse warning: %v", res.Steps)
	}
	// Without caller copy the reuse is silent, as before.
	c2 := newAPIClient(t, api.handler(t))
	res2, err := c2.CreateCampaign(context.Background(), validInput())
	if err != nil {
		t.Fatalf("CreateCampaign without copy: %v", err)
	}
	if stepsContain(res2.Steps, "caller ad copy NOT applied") {
		t.Errorf("reuse warning emitted with no caller copy: %v", res2.Steps)
	}
}

// TestValidateCallerAdCopy holds the strict caller rules: counts, no blank or padded entries,
// the width-aware caps, uniqueness and the path rules, each naming its request field.
func TestValidateCallerAdCopy(t *testing.T) {
	h3 := []string{"Alpha", "Beta", "Gamma"}
	d2 := []string{"First description.", "Second description."}
	cases := map[string]struct {
		h, d       []string
		p1, p2     string
		field, msg string
	}{
		"ok":                   {h: h3, d: d2, p1: "kubecon", p2: "register"},
		"omitted":              {},
		"two headlines":        {h: h3[:2], field: FieldHeadlines, msg: "must have 3 to 15 headlines, got 2"},
		"sixteen headlines":    {h: strings.Split("a b c d e f g h i j k l m n o p", " "), field: FieldHeadlines, msg: "must have 3 to 15 headlines, got 16"},
		"one description":      {d: d2[:1], field: FieldDescriptions, msg: "must have 2 to 4 descriptions, got 1"},
		"five descriptions":    {d: []string{"a", "b", "c", "d", "e"}, field: FieldDescriptions, msg: "got 5"},
		"empty headline":       {h: []string{"Alpha", "", "Gamma"}, field: FieldHeadlines, msg: "headline 2 must contain at least one word"},
		"blank description":    {d: []string{"First.", "  "}, field: FieldDescriptions, msg: "description 2 must contain at least one word"},
		"headline 31":          {h: []string{strings.Repeat("a", 31), "b", "c"}, field: FieldHeadlines, msg: "exceeds 30"},
		"wide headline 16":     {h: []string{strings.Repeat("東", 16), "b", "c"}, field: FieldHeadlines, msg: "exceeds 15"},
		"description 91":       {d: []string{strings.Repeat("a", 91), "b"}, field: FieldDescriptions, msg: "exceeds 90"},
		"duplicate headline":   {h: []string{"Alpha", "ALPHA", "Gamma"}, field: FieldHeadlines, msg: "duplicate"},
		"control in headline":  {h: []string{"Al\npha", "Beta", "Gamma"}, field: FieldHeadlines, msg: "control character"},
		"path2 without path1":  {p2: "x", field: FieldPath2, msg: "requires path1"},
		"path1 over cap":       {p1: strings.Repeat("a", 16), field: FieldPath1, msg: "exceeding 15"},
		"path2 slash":          {p1: "a", p2: "b/c", field: FieldPath2, msg: "forward slash"},
		"headline 30 accepted": {h: []string{strings.Repeat("a", 30), "b", "c"}},
		"path 15 accepted":     {p1: strings.Repeat("a", 15), p2: strings.Repeat("b", 15)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateCallerAdCopy(tc.h, tc.d, tc.p1, tc.p2)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("ValidateCallerAdCopy: %v", err)
				}
				return
			}
			var ce *AdCopyError
			if !errors.As(err, &ce) {
				t.Fatalf("error %v is not an *AdCopyError", err)
			}
			if ce.Field != tc.field || !strings.Contains(ce.Msg, tc.msg) {
				t.Errorf("got %s: %s; want %s: …%s…", ce.Field, ce.Msg, tc.field, tc.msg)
			}
		})
	}
}

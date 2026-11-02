// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/platformtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// promoScenario drives one way the Reddit promoted-post leg can end, through the real
// dispatcher and client against a fake Reddit, and states what must come out. Every
// expectation is a LITERAL, not read from the client's outcome table, so a change to the
// table's text, its classification order or its result shape fails here, against the row it
// changed, instead of silently flipping a neighbouring case (#173).
type promoScenario struct {
	name string
	want reddit.PromoOutcome
	// leg is "author" (imageUrl: Step 1.5 authors a post, then Step 4 promotes it) or "post"
	// (postUrl: Step 4 promotes the caller's post) or "none" (neither).
	leg      string
	variants bool
	// target is the request whose answer the scenario controls: "/posts" (authoring) or "/ads".
	target string
	// behave is how the target is answered: an HTTP status, "no_id" (2xx without data.id),
	// "hijack" (connection closed mid-flight), "hijack_cancel" (caller cancels, then the
	// connection is closed mid-flight), "refused" (a proven pre-send dial refusal) or
	// "cancel_before" (the caller cancels before the target is sent).
	behave string

	wantPersisted string
	wantAdWarning string
	// wantStep must appear verbatim among the persisted Steps ("" asserts nothing).
	wantStep string
	// wantNoStep must appear in no persisted Step.
	wantNoStep []string
}

const (
	campaignContextStep = "Campaign cmp_1 and ad group ag_1 were created (PAUSED) with no ad -- add the ad to THIS campaign in Reddit Ads Manager rather than creating a new one"
	campaignContextWarn = " (the campaign and ad group were created and are PAUSED)"
)

var promoScenarios = []promoScenario{
	{
		name: "nothing requested, no variants", want: reddit.PromoNotRequestedNoVariants, leg: "none",
		wantPersisted: "created",
		wantStep:      "No ad variants or post URL provided -- add ads manually in Reddit Ads Manager",
	},
	{
		name: "nothing requested, variants listed", want: reddit.PromoNotRequestedVariants, leg: "none", variants: true,
		wantPersisted: "created",
		wantStep:      "1 ad variant(s) ready -- create ads in Reddit Ads Manager with these headlines (set/replace the shown utm_* params on your registration URL, keeping all its other query parameters; drop any trailing '/' from the path so it matches the automated destination):",
	},
	{
		name: "ad created from a caller post", want: reddit.PromoAdCreated, leg: "post", target: "/ads", behave: "200",
		wantPersisted: "created",
	},
	{
		name: "ad created from an authored post", want: reddit.PromoAdCreated, leg: "author", target: "/ads", behave: "200",
		wantPersisted: "created",
		wantStep:      "Authored promoted post: t3_post1 (CTA: Learn More)",
	},

	// Step 1.5: authoring.
	{
		name: "authoring interrupted in flight by the caller", want: reddit.PromoAuthorCanceledInFlight, leg: "author", target: "/posts", behave: "hijack_cancel",
		wantPersisted: "pending",
		wantStep:      "Promoted-post authoring is UNCONFIRMED (the request reached Reddit but the outcome is unknown) -- no campaign was created; check Reddit Ads Manager for a post with this headline BEFORE authoring it again to avoid a duplicate, and DELETE it if it exists and is not attached to an ad",
	},
	{
		name: "authoring cancelled before send", want: reddit.PromoAuthorCanceledBeforeSend, leg: "author", target: "/posts", behave: "cancel_before",
		wantPersisted: "not_created",
	},
	{
		name: "authoring 5xx", want: reddit.PromoAuthorUnconfirmed, leg: "author", target: "/posts", behave: "503",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring is UNCONFIRMED (the request may have reached Reddit) and no ad was created — verify in Reddit Ads Manager BEFORE authoring the post again to avoid a duplicate, and DELETE any post that is not attached to an ad" + campaignContextWarn,
		wantStep:      "Promoted-post authoring UNCONFIRMED (HTTP 503) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
		wantNoStep:    []string{"author the post and add the ad", "ad variant(s) ready", "add ads manually"},
	},
	{
		// The ordering trap of #158 round 2: an apiError that is ALSO ambiguous.
		name: "authoring mutating 3xx", want: reddit.PromoAuthorUnconfirmed, leg: "author", target: "/posts", behave: "302",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring is UNCONFIRMED (the request may have reached Reddit) and no ad was created — verify in Reddit Ads Manager BEFORE authoring the post again to avoid a duplicate, and DELETE any post that is not attached to an ad" + campaignContextWarn,
		wantStep:      "Promoted-post authoring UNCONFIRMED (HTTP 302) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
	},
	{
		name: "authoring 429", want: reddit.PromoAuthorUnconfirmed, leg: "author", target: "/posts", behave: "429",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring is UNCONFIRMED (the request may have reached Reddit) and no ad was created — verify in Reddit Ads Manager BEFORE authoring the post again to avoid a duplicate, and DELETE any post that is not attached to an ad" + campaignContextWarn,
		wantStep:      "Promoted-post authoring UNCONFIRMED (HTTP 429) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
	},
	{
		name: "authoring transport error", want: reddit.PromoAuthorUnconfirmed, leg: "author", target: "/posts", behave: "hijack",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring is UNCONFIRMED (the request may have reached Reddit) and no ad was created — verify in Reddit Ads Manager BEFORE authoring the post again to avoid a duplicate, and DELETE any post that is not attached to an ad" + campaignContextWarn,
		wantStep:      "Promoted-post authoring UNCONFIRMED (the request may have reached Reddit) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
	},
	{
		name: "authoring 2xx without a post id", want: reddit.PromoAuthorUnconfirmed, leg: "author", target: "/posts", behave: "no_id",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring is UNCONFIRMED (the request may have reached Reddit) and no ad was created — verify in Reddit Ads Manager BEFORE authoring the post again to avoid a duplicate, and DELETE any post that is not attached to an ad" + campaignContextWarn,
		wantStep:      "Promoted-post authoring UNCONFIRMED (the request may have reached Reddit) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
	},
	{
		name: "authoring rejected 4xx", want: reddit.PromoAuthorRejected, leg: "author", target: "/posts", behave: "400",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring FAILED (Reddit rejected the post) and no ad was created — author the post and add the ad in Reddit Ads Manager" + campaignContextWarn,
		wantStep:      "Promoted-post authoring failed: Reddit rejected the post (HTTP 400) -- continuing without an ad; author the post and add the ad in Reddit Ads Manager",
		wantNoStep:    []string{"UNCONFIRMED", "ad variant(s) ready"},
	},
	{
		name: "authoring refused before send", want: reddit.PromoAuthorPreSend, leg: "author", target: "/posts", behave: "refused",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post authoring FAILED before it reached Reddit; no post exists and no ad was created — author the post and add the ad in Reddit Ads Manager" + campaignContextWarn,
		wantStep:      "Promoted-post authoring failed before the request reached Reddit -- continuing without an ad; author the post and add the ad in Reddit Ads Manager",
		wantNoStep:    []string{"UNCONFIRMED"},
	},

	// Step 4: the ad.
	{
		name: "ad interrupted in flight by the caller", want: reddit.PromoAdCanceledInFlight, leg: "post", target: "/ads", behave: "hijack_cancel",
		wantPersisted: "pending",
		wantAdWarning: "promoted-post ad is UNCONFIRMED: the create was interrupted in flight and the ad MAY exist — verify in Reddit Ads Manager before recreating",
	},
	{
		name: "ad cancelled before send", want: reddit.PromoAdCanceledBeforeSend, leg: "post", target: "/ads", behave: "cancel_before",
		wantPersisted: "pending",
	},
	{
		name: "ad 5xx", want: reddit.PromoAdUnconfirmed, leg: "post", target: "/ads", behave: "500",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post ad creation is UNCONFIRMED (campaign and ad group created, PAUSED); the ad request may have reached Reddit — verify whether the ad exists BEFORE creating it manually to avoid a duplicate",
		wantStep:      "Ad creation UNCONFIRMED (HTTP 500) -- verify in Reddit Ads Manager before adding manually",
	},
	{
		name: "ad mutating 3xx", want: reddit.PromoAdUnconfirmed, leg: "post", target: "/ads", behave: "307",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post ad creation is UNCONFIRMED (campaign and ad group created, PAUSED); the ad request may have reached Reddit — verify whether the ad exists BEFORE creating it manually to avoid a duplicate",
		wantStep:      "Ad creation UNCONFIRMED (HTTP 307) -- verify in Reddit Ads Manager before adding manually",
	},
	{
		name: "ad transport error", want: reddit.PromoAdUnconfirmed, leg: "post", target: "/ads", behave: "hijack",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post ad creation is UNCONFIRMED (campaign and ad group created, PAUSED); the ad request may have reached Reddit — verify whether the ad exists BEFORE creating it manually to avoid a duplicate",
		wantStep:      "Ad creation UNCONFIRMED (request may have reached Reddit) -- verify in Reddit Ads Manager before adding manually",
	},
	{
		name: "ad 2xx without an ad id", want: reddit.PromoAdNoID, leg: "post", target: "/ads", behave: "no_id",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post ad creation is UNCONFIRMED (2xx response with no ad id); the ad may already exist — verify in Reddit Ads Manager BEFORE creating it manually to avoid a duplicate",
		wantStep:      "Ad creation UNCONFIRMED (2xx with no ad ID, post: t3_abc123) -- verify in Reddit Ads Manager before adding manually",
	},
	{
		name: "ad rejected 4xx", want: reddit.PromoAdRejected, leg: "post", target: "/ads", behave: "400",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post ad creation FAILED (campaign and ad group created, PAUSED); Reddit rejected the ad — add it manually in Reddit Ads Manager",
		wantStep:      "Ad creation failed: Reddit rejected the ad (HTTP 400) -- add ad manually in Reddit Ads Manager",
	},
	{
		name: "ad refused before send", want: reddit.PromoAdPreSend, leg: "post", target: "/ads", behave: "refused",
		wantPersisted: "created_degraded",
		wantAdWarning: "promoted-post ad creation FAILED before it reached Reddit (campaign and ad group created, PAUSED); the ad was not created — add it manually in Reddit Ads Manager",
		wantStep:      "Ad creation failed before the request reached Reddit -- add ad manually in Reddit Ads Manager",
	},
}

// TestReddit_PromoOutcomeTable pins every row of the Reddit promoted-post outcome table
// (internal/platform/reddit/promo_outcome.go) end to end: the persisted status the dispatcher
// writes, the AdWarning and the runbook step. It also fails when the table gains a row no
// scenario drives, or a row's Persisted column disagrees with what the dispatcher writes.
func TestReddit_PromoOutcomeTable(t *testing.T) {
	covered := map[reddit.PromoOutcome]bool{}
	for _, sc := range promoScenarios {
		covered[sc.want] = true
	}
	rows := map[reddit.PromoOutcome]reddit.PromoOutcomeRow{}
	for _, row := range reddit.PromoOutcomes() {
		rows[row.Outcome] = row
		if !covered[row.Outcome] {
			t.Errorf("outcome %q has a table row but no scenario; every row must be driven end to end", row.Outcome)
		}
	}

	for _, sc := range promoScenarios {
		t.Run(sc.name, func(t *testing.T) {
			row, ok := rows[sc.want]
			if !ok {
				t.Fatalf("scenario names outcome %q, which the table does not have", sc.want)
			}
			if got := persistedLabel(row.Persisted); got != sc.wantPersisted {
				t.Errorf("table row %q says persisted %q, scenario expects %q", sc.want, row.Persisted, sc.wantPersisted)
			}

			camp, err := runPromoScenario(t, sc)
			var gotPersisted string
			var pce *preCreateError
			switch {
			case err == nil:
				gotPersisted = camp.Status
			case errors.As(err, &pce):
				gotPersisted = "not_created"
				if camp != nil {
					t.Errorf("a pre-create error must carry no campaign, got %+v", camp)
				}
			case camp != nil:
				// The orchestrator persists a (campaign, error) return as a pending orphan
				// and retains the claim.
				gotPersisted = "pending"
			default:
				t.Fatalf("Dispatch = (nil, %v), which is neither a pre-create error nor a retained partial", err)
			}
			if gotPersisted != sc.wantPersisted {
				t.Fatalf("persisted = %q (err %v), want %q", gotPersisted, err, sc.wantPersisted)
			}
			if camp == nil {
				return
			}

			var res reddit.CampaignResult
			if jerr := json.Unmarshal(camp.Result, &res); jerr != nil {
				t.Fatalf("decode persisted result: %v", jerr)
			}
			if res.AdWarning != sc.wantAdWarning {
				t.Errorf("AdWarning =\n  %q\nwant\n  %q", res.AdWarning, sc.wantAdWarning)
			}
			if sc.wantStep != "" && !containsStep(res.Steps, sc.wantStep) {
				t.Errorf("Steps missing %q:\n  %s", sc.wantStep, strings.Join(res.Steps, "\n  "))
			}
			for _, bad := range sc.wantNoStep {
				for _, s := range res.Steps {
					if strings.Contains(s, bad) {
						t.Errorf("step %q contains %q, which contradicts this outcome", s, bad)
					}
				}
			}
			// The deferred campaign context belongs to exactly the non-fatal authoring rows.
			if want := row.CampaignContext; containsStep(res.Steps, campaignContextStep) != want {
				t.Errorf("campaign-context step present = %v, want %v: %s", !want, want, strings.Join(res.Steps, " | "))
			}
			if wantAd := sc.want == reddit.PromoAdCreated; (res.AdCount == 1) != wantAd {
				t.Errorf("AdCount = %d, want ad created = %v", res.AdCount, wantAd)
			}
		})
	}
}

func persistedLabel(p string) string {
	switch p {
	case reddit.PersistedNotCreated:
		return "not_created"
	case reddit.PersistedPending:
		return "pending"
	case reddit.PersistedCreatedDegraded:
		return campaignStatusCreatedDegraded
	case reddit.PersistedCreated:
		return campaignStatusCreated
	}
	return "unknown:" + p
}

func containsStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

// promoTransport cancels the caller's context or refuses the dial around the scenario's target.
type promoTransport struct {
	base    http.RoundTripper
	refused http.RoundTripper
	sc      promoScenario
	cancel  context.CancelFunc
}

func (p *promoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	isTarget := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, p.sc.target)
	if isTarget && p.sc.behave == "refused" {
		dead, err := url.Parse(platformtest.RefusedURL)
		if err != nil {
			return nil, err
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = dead.Scheme, dead.Host
		return p.refused.RoundTrip(clone)
	}
	resp, err := p.base.RoundTrip(r)
	if err != nil || p.sc.behave != "cancel_before" {
		return resp, err
	}
	// Cancel once the request BEFORE the target has been fully answered, so the target is
	// the first request to observe the cancellation and none of its bytes are sent.
	before := (p.sc.target == "/posts" && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/ad_accounts/")) ||
		(p.sc.target == "/ads" && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ad_groups"))
	if before {
		body, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		p.cancel()
	}
	return resp, nil
}

func runPromoScenario(t *testing.T, sc promoScenario) (*model.Campaign, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.Method == http.MethodPost && sc.target != "" && strings.HasSuffix(path, sc.target) && sc.behave != "200" {
			switch sc.behave {
			case "no_id":
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
			case "hijack", "hijack_cancel":
				if sc.behave == "hijack_cancel" {
					cancel()
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
			case "cancel_before", "refused":
				t.Errorf("%s: the target request must never reach Reddit", sc.behave)
				w.WriteHeader(http.StatusInternalServerError)
			default:
				code := 0
				for _, c := range sc.behave {
					code = code*10 + int(c-'0')
				}
				if code >= 300 && code < 400 {
					w.Header().Set("Location", "https://ads-api.reddit.com/elsewhere")
				}
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":"no"}`)
			}
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/ad_accounts/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_acct"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/posts"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t3_post1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ads"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)

	tr := &promoTransport{base: http.DefaultTransport.(*http.Transport).Clone(), sc: sc, cancel: cancel}
	if sc.behave == "refused" {
		tr.refused = platformtest.RefusedDialTransport(t)
	}
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithHTTPClient(&http.Client{Transport: tr}),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)

	rc := map[string]any{
		"budgetUsd": 50, "startDate": "2099-08-01", "endDate": "2099-08-31",
		"objective": "traffic", "subreddits": []string{"kubernetes"},
	}
	switch sc.leg {
	case "author":
		rc["imageUrl"] = "https://events.linuxfoundation.org/banner.jpg"
	case "post":
		rc["postUrl"] = "t3_abc123"
	}
	if sc.variants {
		rc["variants"] = []map[string]string{{"headline": "Join us"}}
	}
	cfg, err := json.Marshal(map[string]any{"redditConfig": rc})
	if err != nil {
		t.Fatal(err)
	}
	return d.Dispatch(ctx, testBrief(), model.ProviderRedditAds, cfg)
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// campaignByNameQueryMarker is the part of FindCampaignByName's GAQL that no other query in
// this package carries. refuseIfCampaignExists sends it before every create's first write.
const campaignByNameQueryMarker = "FROM campaign WHERE campaign.name = "

// noExistingCampaignTransport answers the create path's pre-write campaign lookup
// (refuseIfCampaignExists) with an honoured empty page — "no campaign of this name" — and
// passes every other request through untouched.
//
// It sits in the TRANSPORT rather than in each fake's handler so the fakes that count or
// order their googleAds:search answers (the budget-reuse and ambiguous-create tests) see
// exactly the requests they did before the check existed. Tests about the check itself do
// not use it.
type noExistingCampaignTransport struct{ base http.RoundTripper }

func (t noExistingCampaignTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "googleAds:search") && r.Body != nil {
		raw, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		if bytes.Contains(raw, []byte(campaignByNameQueryMarker)) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"results":[]}`)),
				Request:    r,
			}, nil
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// noExistingCampaign makes c's pre-write campaign lookup see an empty account, for tests
// about everything that happens after it. Returns c for chaining at construction.
func noExistingCampaign(c *Client) *Client {
	hc := *c.httpClient
	hc.Transport = noExistingCampaignTransport{base: hc.Transport}
	c.httpClient = &hc
	return c
}

// existingCampaignBudgetID is the budget the already-existing campaign is attached to in
// these tests. It differs from okBudget's 111, so a channel that created a budget anyway
// cannot pass by reporting that one.
const existingCampaignBudgetID = "16006171193"

// existingCampaignServer is an account in which a live campaign of the composed name
// already exists, attached to existingCampaignBudgetID: the state a retry meets after a
// create that succeeded. Images are served from images. Every mutate is recorded and
// refused, so a test fails if the create writes ANYTHING, budget or otherwise.
func existingCampaignServer(t *testing.T, campaignName string, images map[string][]byte) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		mutates []string
	)
	h := func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			raw, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case bytes.Contains(raw, []byte(campaignByNameQueryMarker)):
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{lookupRow("24433950855", campaignName, StatusPaused)}})
			case bytes.Contains(raw, []byte("campaign.id = 24433950855")):
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{json.RawMessage(
					`{"campaign":{"resourceName":"customers/1234567890/campaigns/24433950855","id":"24433950855",` +
						`"name":` + jsonQuote(campaignName) + `,"status":"PAUSED","advertisingChannelType":"DEMAND_GEN"},` +
						`"campaignBudget":{"id":"` + existingCampaignBudgetID + `","amountMicros":"5000000","period":"DAILY",` +
						`"deliveryMethod":"STANDARD","explicitlyShared":false}}`)}})
			default:
				t.Errorf("unexpected search: %s", raw)
				w.WriteHeader(http.StatusInternalServerError)
			}
		default:
			mu.Lock()
			mutates = append(mutates, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	srv := demandGenTLSServer(t, h)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), mutates...)
	}
}

// assertCampaignNameTakenWithoutWrites is the one assertion every channel's retry test
// makes: no mutate of any kind (and so no campaignBudgets:mutate), the outcome is
// campaign_name_taken, and the partial carries the EXISTING campaign's budget, never a
// campaign id the brief never asked to adopt.
func assertCampaignNameTakenWithoutWrites(t *testing.T, res *CampaignResult, err error, mutates []string) {
	t.Helper()
	if err == nil {
		t.Fatal("a create meeting an existing campaign of its name must fail")
	}
	if len(mutates) != 0 {
		t.Errorf("no write may be sent when the campaign already exists, got %v", mutates)
	}
	if res == nil {
		t.Fatalf("an existing campaign must be reported as a partial, not a pre-create failure: %v", err)
	}
	if res.CampaignBudgetID != existingCampaignBudgetID {
		t.Errorf("CampaignBudgetID = %q, want the existing campaign's budget %s", res.CampaignBudgetID, existingCampaignBudgetID)
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q: the client must not bind the brief to a campaign it did not create", res.CampaignID)
	}
	if got, ok := CreateOutcome(res, err); !ok || got != domain.CreateOutcomeCampaignNameTaken {
		t.Errorf("CreateOutcome = %q, %v; want %q", got, ok, domain.CreateOutcomeCampaignNameTaken)
	}
}

// existingCampaignClient is demandGenClient for existingCampaignServer, with no lookup
// interception: the lookup is what these tests are about.
func existingCampaignClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := demandGenClient(t, srv)
	// demandGenClient answers the lookup with an empty account for every other test; undo
	// that here.
	tr, ok := c.httpClient.Transport.(noExistingCampaignTransport)
	if !ok {
		t.Fatalf("demandGenClient's transport is %T, want noExistingCampaignTransport to unwrap", c.httpClient.Transport)
	}
	hc := *c.httpClient
	hc.Transport = tr.base
	c.httpClient = &hc
	return c
}

func TestCreateCampaign_ExistingCampaignCreatesNoBudget(t *testing.T) {
	in := sampleInput()
	srv, mutates := existingCampaignServer(t, ComposeName(CampaignKindSearch, in), nil)
	res, err := existingCampaignClient(t, srv).CreateCampaign(context.Background(), in)
	assertCampaignNameTakenWithoutWrites(t, res, err, mutates())
}

// Demand Gen writes its demographic Audiences BEFORE the budget, so the check has to run
// before those too. Without Audiences the first write a retry would send is the budget
// create (the #540 orphan); with them, it is audiences:mutate. Neither may be sent.
func TestCreateDemandGenCampaign_ExistingCampaignCreatesNoBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		audiences bool
	}{{"no Audiences", false}, {"with Audiences", true}} {
		t.Run(tc.name, func(t *testing.T) {
			input := func(base string) CampaignInput {
				in := creativeInput(creativeAt(base))
				if tc.audiences {
					in.DemandGenDemographics = fullDemographics()
				}
				return in
			}
			srv, mutates := existingCampaignServer(t, ComposeName(CampaignKindDemandGen, input("https://unused.invalid")), twoImages(t))
			res, err := existingCampaignClient(t, srv).CreateDemandGenCampaign(context.Background(), input(srv.URL))
			assertCampaignNameTakenWithoutWrites(t, res, err, mutates())
		})
	}
}

func TestCreateDisplayCampaign_ExistingCampaignCreatesNoBudget(t *testing.T) {
	in := displayInput("https://unused.invalid")
	srv, mutates := existingCampaignServer(t, ComposeName(CampaignKindDisplay, in), displayImages(t))
	res, err := existingCampaignClient(t, srv).CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	assertCampaignNameTakenWithoutWrites(t, res, err, mutates())
}

func TestCreatePerformanceMaxCampaign_ExistingCampaignCreatesNoBudget(t *testing.T) {
	in := pmaxInput(pmaxCreativeAt("https://unused.invalid"))
	srv, mutates := existingCampaignServer(t, ComposeName(CampaignKindPerformanceMax, in), pmaxImages(t))
	res, err := existingCampaignClient(t, srv).CreatePerformanceMaxCampaign(context.Background(), pmaxInput(pmaxCreativeAt(srv.URL)))
	assertCampaignNameTakenWithoutWrites(t, res, err, mutates())
	if res != nil && !slices.Contains(res.Steps, expansionActionRequiredIfExists) {
		t.Errorf("an existing Performance Max campaign may predate the final-URL-expansion opt-out; steps = %v, want the ACTION REQUIRED step", res.Steps)
	}
}

func TestCreateVideoCampaignCascade_ExistingCampaignCreatesNoBudget(t *testing.T) {
	in := videoInput()
	srv, mutates := existingCampaignServer(t, ComposeName(CampaignKindVideo, in), nil)
	res, err := existingCampaignClient(t, srv).createVideoCampaignCascade(context.Background(), in)
	assertCampaignNameTakenWithoutWrites(t, res, err, mutates())
}

// A lookup that cannot be verified refuses the create before any write, as a pre-create
// failure: nothing was sent, so the claim is released and a retry asks again. Going ahead
// would rebuild the orphan the check exists to prevent.
func TestCreateCampaign_UnverifiableCampaignLookupSendsNoWrite(t *testing.T) {
	var (
		mu      sync.Mutex
		mutates []string
	)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		mutates = append(mutates, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	res, err := existingCampaignClient(t, srv).CreateCampaign(context.Background(), sampleInput())
	if err == nil || res != nil {
		t.Fatalf("an unverifiable lookup must refuse pre-create, got res=%v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "campaign creation not attempted") {
		t.Errorf("err = %v, want it to say the create was not attempted", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mutates) != 0 {
		t.Errorf("no write may be sent on an unverifiable lookup, got %v", mutates)
	}
}

// A campaign found by name whose budget cannot be read is also refused before any write,
// rather than reported with an empty budget id or created around.
func TestCreateCampaign_ExistingCampaignWithUnreadableBudgetSendsNoWrite(t *testing.T) {
	in := sampleInput()
	name := ComposeName(CampaignKindSearch, in)
	var (
		mu      sync.Mutex
		mutates []string
	)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			raw, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			if bytes.Contains(raw, []byte(campaignByNameQueryMarker)) {
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{lookupRow("24433950855", name, StatusPaused)}})
				return
			}
			// The settings row with no campaignBudget at all.
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{json.RawMessage(
				`{"campaign":{"resourceName":"customers/1234567890/campaigns/24433950855","id":"24433950855","name":` +
					jsonQuote(name) + `,"status":"PAUSED"}}`)}})
			return
		}
		mu.Lock()
		mutates = append(mutates, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	res, err := existingCampaignClient(t, srv).CreateCampaign(context.Background(), in)
	if err == nil || res != nil {
		t.Fatalf("an existing campaign with no readable budget must refuse pre-create, got res=%v err=%v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mutates) != 0 {
		t.Errorf("no write may be sent, got %v", mutates)
	}
}

// A campaign found live by name and then read back REMOVED (gone between the two reads) is
// refused before any write too: there is no live campaign to report a budget for, and a
// retry will see the account as it now is.
func TestCreateCampaign_ExistingCampaignRemovedBetweenReadsSendsNoWrite(t *testing.T) {
	in := sampleInput()
	name := ComposeName(CampaignKindSearch, in)
	var (
		mu      sync.Mutex
		mutates []string
	)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			raw, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			if bytes.Contains(raw, []byte(campaignByNameQueryMarker)) {
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{lookupRow("24433950855", name, StatusPaused)}})
				return
			}
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{json.RawMessage(
				`{"campaign":{"resourceName":"customers/1234567890/campaigns/24433950855","id":"24433950855","name":` +
					jsonQuote(name) + `,"status":"REMOVED"},"campaignBudget":{"id":"` + existingCampaignBudgetID + `"}}`)}})
			return
		}
		mu.Lock()
		mutates = append(mutates, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	res, err := existingCampaignClient(t, srv).CreateCampaign(context.Background(), in)
	if err == nil || res != nil {
		t.Fatalf("a campaign removed between the reads must refuse pre-create, got res=%v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "absent or removed") {
		t.Errorf("err = %v, want it to say the campaign is now removed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mutates) != 0 {
		t.Errorf("no write may be sent, got %v", mutates)
	}
}

// A context that ends during the campaign-name read is a pre-create refusal for every
// channel, not a first mutate sent on a dead context (which would read as ambiguous).
func TestRefuseIfCampaignExists_ContextDoneAfterReadIsPreCreate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	c := existingCampaignClient(t, srv)
	// The read answers in full, then the deadline passes before the caller's next step.
	hc := *c.httpClient
	hc.Transport = cancelAfterSearchTransport{base: hc.Transport, cancel: cancel}
	c.httpClient = &hc

	res, err := c.refuseIfCampaignExists(ctx, "demand gen", "LFX | DemandGen Campaign | x", func() *CampaignResult { return &CampaignResult{} })
	if res != nil || err == nil {
		t.Fatalf("got res=%v err=%v, want a pre-create (nil, err)", res, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// cancelAfterSearchTransport buffers a googleAds:search response whole, then cancels: the
// read succeeds and the context is done by the time the caller looks at it again.
type cancelAfterSearchTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (t cancelAfterSearchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil || !strings.HasSuffix(r.URL.Path, "googleAds:search") {
		return resp, err
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	t.cancel()
	return resp, nil
}

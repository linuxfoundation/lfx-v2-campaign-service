// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// unauthorizedBody is the 401 payload LinkedIn returns for an expired token. It is the
// body isTokenExpiryResponse classifies, so using it keeps these tests on the real
// production arm rather than a synthetic one.
const unauthorizedBody = `{"serviceErrorCode":65602,"message":"The token used in the request has expired","status":401}`

// TestCreateOutcomeAmbiguous_401IsMethodGated pins the CLASSIFIER directly: a
// credentialsExpiredError is outcome-ambiguous exactly when it answered a MUTATING
// request, and definite otherwise.
//
// LinkedIn "reserves the right to revoke Refresh Tokens or Access Tokens at any time",
// so a revocation can take effect between LinkedIn committing a create POST and writing
// its response. That makes a 401 on a POST say nothing about whether the write landed —
// the same position a mutating 5xx leaves the caller in. A GET 401 created nothing, and
// the PRE-SEND expiry arms never sent a request at all (Method == ""), so both must stay
// definite failures.
//
// Table-driven across the method/no-method combinations rather than reasoning about
// them: the zero-Method case is the one a "carry the status code" fix would most easily
// get wrong, and it is the case every pre-send arm in token.go produces.
func TestCreateOutcomeAmbiguous_401IsMethodGated(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "POST 401 is ambiguous — the create may have committed before the token died",
			err:  &credentialsExpiredError{Connection: "c", Reason: "r", Method: http.MethodPost, StatusCode: http.StatusUnauthorized},
			want: true,
		},
		{
			name: "PUT 401 is ambiguous",
			err:  &credentialsExpiredError{Connection: "c", Reason: "r", Method: http.MethodPut, StatusCode: http.StatusUnauthorized},
			want: true,
		},
		{
			name: "DELETE 401 is ambiguous",
			err:  &credentialsExpiredError{Connection: "c", Reason: "r", Method: http.MethodDelete, StatusCode: http.StatusUnauthorized},
			want: true,
		},
		{
			name: "GET 401 created nothing, so it stays a definite expiry",
			err:  &credentialsExpiredError{Connection: "c", Reason: "r", Method: http.MethodGet, StatusCode: http.StatusUnauthorized},
			want: false,
		},
		{
			name: "pre-send expiry (no method, no status) is definite — nothing was sent",
			err:  &credentialsExpiredError{Connection: "c", Reason: "the refresh token expired, so a new access token cannot be minted"},
			want: false,
		},
		{
			name: "a wrapped POST 401 is still found through the chain",
			err:  &partialCascadeError{stage: "creative", err: &credentialsExpiredError{Connection: "c", Reason: "r", Method: http.MethodPost, StatusCode: http.StatusUnauthorized}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createOutcomeAmbiguous(tc.err); got != tc.want {
				t.Errorf("createOutcomeAmbiguous = %v, want %v", got, tc.want)
			}
			// IsOutcomeUnconfirmed is the exported surface the dispatcher reads; for the
			// non-partial cases it must agree exactly with the classifier.
			if _, partial := tc.err.(*partialCascadeError); !partial {
				if got := IsOutcomeUnconfirmed(tc.err); got != tc.want {
					t.Errorf("IsOutcomeUnconfirmed = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestDoRequest_401CarriesMethodAndStatus proves the wrap site actually POPULATES the
// fields the classifier reads, over a real HTTP round-trip. Asserting the classifier in
// isolation (above) would pass equally well against a doRequest that still dropped them.
func TestDoRequest_401CarriesMethodAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, unauthorizedBody)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			_, err := c.doRequest(context.Background(), method, "adCampaignGroups", map[string]any{"a": 1}, nil, nil)
			var ce *credentialsExpiredError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want a *credentialsExpiredError", err)
			}
			if ce.Method != method {
				t.Errorf("Method = %q, want %q — the 401 wrap site must carry the in-scope method", ce.Method, method)
			}
			if ce.StatusCode != http.StatusUnauthorized {
				t.Errorf("StatusCode = %d, want %d — the 401 wrap site must carry the observed status", ce.StatusCode, http.StatusUnauthorized)
			}
			// The actionable half of the 401 is unchanged: it is still an expiry.
			if !errors.Is(err, ErrCredentialsExpired) {
				t.Errorf("err = %v, want ErrCredentialsExpired — carrying the method must not cost the reconnect signal", err)
			}
			// And the classification half now works.
			wantAmbiguous := method == http.MethodPost
			if got := IsOutcomeUnconfirmed(err); got != wantAmbiguous {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v for a %s 401", got, wantAmbiguous, method)
			}
			// The operator-facing message must NOT grow the method: it stays the single
			// actionable "reconnect" sentence. (The Reason string has always named the
			// HTTP 401, so the status is not what this asserts — the METHOD is, and it
			// is the field a naive "render everything" Error() would leak.)
			if strings.Contains(err.Error(), method) {
				t.Errorf("Error() = %q; the method is carried for classification and must not be rendered", err.Error())
			}
		})
	}
}

// TestCreateCampaign_GroupPOST401ReturnsUnconfirmedPartial is the end-to-end statement of
// the defect. A 401 answering the campaign-GROUP create POST may follow a group LinkedIn
// already committed — the group is a permanent, billable resource carrying the
// deterministic name — so CreateCampaign must return a NON-NIL partial result plus an
// UNCONFIRMED error, not the clean (nil, err) that tells the orchestrator to release the
// dispatch claim.
//
// The non-nil result is the load-bearing assertion: the dispatcher's claim-retention rule
// keys on `result == nil` alone (never on whether an id is populated), so a nil here
// releases the claim and orphans the group.
func TestCreateCampaign_GroupPOST401ReturnsUnconfirmedPartial(t *testing.T) {
	var mu sync.Mutex
	var groupPosts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The find-existing lookups are GETs and must succeed, so the flow reaches the
		// create POST — otherwise the test would pass for the wrong reason (a GET failure
		// is correctly NOT an ambiguous create).
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		if strings.Contains(r.URL.Path, "adCampaignGroups") {
			mu.Lock()
			groupPosts++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, unauthorizedBody)
			return
		}
		t.Errorf("unexpected request after the group 401: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), validCampaignInput())

	if err == nil {
		t.Fatal("expected an error when the group create POST is answered 401")
	}
	if res == nil {
		t.Fatal("result = nil; want a NON-NIL partial result — a 401 can follow a committed " +
			"group create, and the dispatcher retains the dispatch claim on result != nil alone, " +
			"so a nil here releases the claim and orphans a billable campaign group")
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("err = %v; want IsOutcomeUnconfirmed — the group create outcome is genuinely unknowable", err)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("err = %q; want the UNCONFIRMED wording that tells the caller to verify before recreating", err)
	}
	// The reconnect signal survives alongside the ambiguity — both facts, not one.
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Errorf("err = %v; want ErrCredentialsExpired to remain reachable so the caller can still be told to reconnect", err)
	}
	// The partial result must name the group so an operator can find it by name.
	if res.CampaignGroupName == "" {
		t.Error("partial result must carry the deterministic group name — it is the only handle for reconciling the possibly-created group")
	}
	// Fail closed: no replay of the rejected POST (the repo's standing 401 contract).
	mu.Lock()
	defer mu.Unlock()
	if groupPosts != 1 {
		t.Errorf("group POSTs = %d, want 1: a 401 must not be replayed inside the failing operation", groupPosts)
	}
}

// TestCreateCampaign_GETSearch401StaysDefinite is the negative half, and it is what keeps
// the fix from becoming "every 401 is ambiguous". The find-existing lookup is a GET: it
// ran no POST, so nothing was created, and reporting a phantom group would send an
// operator hunting for a resource that does not exist.
func TestCreateCampaign_GETSearch401StaysDefinite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("no POST should be reached: the GET search fails first (%s)", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, unauthorizedBody)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), validCampaignInput())

	if err == nil {
		t.Fatal("expected an error when the group search is answered 401")
	}
	if res != nil {
		t.Errorf("result = %+v; want nil — a GET 401 created nothing, so reporting a partial "+
			"result would claim a resource that does not exist", res)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("err = %v; must NOT be unconfirmed — the search POSTed nothing", err)
	}
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Errorf("err = %v; want ErrCredentialsExpired — a read 401 is still an expiry", err)
	}
}

// validCampaignInput is a fully valid create input, so these tests fail on the injected
// 401 rather than on input validation — which would pass for the wrong reason and prove
// nothing about the 401 classification.
func validCampaignInput() CampaignInput {
	return CampaignInput{
		EventName:        "KubeCon",
		Project:          "tlf",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "join us for a great event", Headline: "KubeCon 2099"}},
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSearchCampaigns_ReturnsEveryMatchInOrder(t *testing.T) {
	var gotPath, gotBody string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"results":[
			{"id":"11","properties":{"hs_name":"KubeCon NA 2026","hs_utm":"kubecon-na-2026","hs_start_date":"2026-11-01"}},
			{"id":"22","properties":{"hs_name":"KubeCon EU 2026","hs_utm":"kubecon-eu-2026","hs_start_date":"2026-03-01"}}
		]}`)
	})

	got, err := c.SearchCampaigns(context.Background(), "  KubeCon  ")
	if err != nil {
		t.Fatalf("SearchCampaigns: %v", err)
	}
	if len(got.Campaigns) != 2 {
		t.Fatalf("matches = %d, want 2", len(got.Campaigns))
	}
	// ORDER is HubSpot's relevance order and must survive: the caller shows these to a human
	// choosing between candidate names, and re-ordering would put a worse match first.
	if got.Campaigns[0].ID != "11" || got.Campaigns[1].ID != "22" {
		t.Errorf("order not preserved: %+v", got.Campaigns)
	}
	// Each field asserted with a DISTINCT value, so a mapper that crossed two of them fails
	// rather than passing against a shared placeholder.
	if got.Campaigns[0].Name != "KubeCon NA 2026" || got.Campaigns[0].UTM != "kubecon-na-2026" || got.Campaigns[0].StartDate != "2026-11-01" {
		t.Errorf("fields not mapped: %+v", got.Campaigns[0])
	}
	if gotPath != campaignSearchPath {
		t.Errorf("path = %q, want %q", gotPath, campaignSearchPath)
	}
	// The query must be TRIMMED on the wire. A padded term fails to match names it should and
	// returns a clean empty answer that reads as "no such campaign".
	var sent struct {
		Query      string   `json:"query"`
		Properties []string `json:"properties"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.Query != "KubeCon" {
		t.Errorf("query = %q, want the trimmed term", sent.Query)
	}
	// The properties must be ASKED FOR. The CRM search returns only system fields otherwise, so
	// a consumer promised a utm token would receive an empty string from every row — and the
	// struct assertions above would still pass against a hand-written fixture.
	for _, want := range []string{"hs_name", "hs_utm", "hs_start_date"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("search does not request %s: %s", want, gotBody)
		}
	}
}

// An empty result is the answer the caller acts on by offering to create a campaign. It must be
// a clean empty slice, never an error.
func TestSearchCampaigns_NoMatchesIsNotAnError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	got, err := c.SearchCampaigns(context.Background(), "nothing matches this")
	if err != nil {
		t.Fatalf("an empty search must not error: %v", err)
	}
	if len(got.Campaigns) != 0 {
		t.Errorf("matches = %d, want 0", len(got.Campaigns))
	}
	if got.Campaigns == nil {
		t.Error("want a non-nil empty slice so the caller need not nil-check")
	}
}

// A malformed 2xx must NOT read as "nothing matched". The caller branches on empty-vs-found to
// decide whether to create a campaign, so a broken response silently answering "empty" would
// prompt a duplicate create in an LF-global namespace.
func TestSearchCampaigns_MalformedBodyIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"results":null}`} {
		t.Run(body, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})

			if _, err := c.SearchCampaigns(context.Background(), "kubecon"); err == nil {
				t.Fatalf("a malformed 2xx (%s) was reported as an empty search", body)
			}
		})
	}
}

// An empty query is not a search for everything: HubSpot would return the whole portal ranked
// arbitrarily and the caller would act on whichever sorted first.
func TestSearchCampaigns_RefusesAnEmptyQueryWithoutCallingHubSpot(t *testing.T) {
	called := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	for _, q := range []string{"", "   "} {
		if _, err := c.SearchCampaigns(context.Background(), q); err == nil {
			t.Errorf("an empty query (%q) was sent", q)
		}
	}
	if called {
		t.Error("HubSpot was contacted for an empty query")
	}
}

// A campaign with no UTM configured is a REAL state and is not "not found". Dropping it would
// hide an existing campaign and prompt a duplicate create.
func TestSearchCampaigns_KeepsACampaignWithNoUTM(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":"11","properties":{"hs_name":"Untokened","hs_utm":""}}]}`)
	})

	got, err := c.SearchCampaigns(context.Background(), "untokened")
	if err != nil {
		t.Fatalf("SearchCampaigns: %v", err)
	}
	if len(got.Campaigns) != 1 {
		t.Fatalf("a campaign with no UTM was dropped: %+v", got.Campaigns)
	}
	if got.Campaigns[0].UTM != "" {
		t.Errorf("UTM = %q, want the empty token carried through", got.Campaigns[0].UTM)
	}
}

// An id-less hit is a MALFORMED response, not a droppable row.
//
// Dropping it would turn a broken payload into a clean empty answer — and empty is what the
// caller acts on by creating a campaign, in a namespace every foundation shares. The paired
// healthy row makes the point sharper: even with a real match present, the response cannot be
// trusted once one hit is unaddressable.
func TestSearchCampaigns_IDLessRowIsAMalformedResponse(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[
			{"id":"","properties":{"hs_name":"Ghost"}},
			{"id":"22","properties":{"hs_name":"Real"}}
		]}`)
	})

	if _, err := c.SearchCampaigns(context.Background(), "x"); err == nil {
		t.Fatal("an id-less hit was silently dropped rather than failing the response")
	}
}

func TestCreateCampaign_ReadsBackTheAssignedToken(t *testing.T) {
	var gotPath, gotMethod, gotBody, gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotQuery = r.URL.Path, r.Method, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// hs_utm is assigned by HubSpot, not sent by us.
		_, _ = io.WriteString(w, `{"id":"99","properties":{"hs_name":"KubeCon NA 2027","hs_utm":"assigned-by-hubspot"}}`)
	})

	got, err := c.CreateCampaign(context.Background(), "  KubeCon NA 2027  ")
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got.ID != "99" {
		t.Errorf("ID = %q, want 99", got.ID)
	}
	// The token must come from the RESPONSE, not be guessed. Asserting a value we never sent is
	// what proves it was read back rather than echoed.
	if got.UTM != "assigned-by-hubspot" {
		t.Errorf("UTM = %q, want the token HubSpot assigned", got.UTM)
	}
	if gotMethod != http.MethodPost || gotPath != campaignCreatePath {
		t.Errorf("%s %s, want POST %s", gotMethod, gotPath, campaignCreatePath)
	}
	if !strings.Contains(gotBody, "KubeCon NA 2027") {
		t.Errorf("name not sent trimmed: %s", gotBody)
	}
	// The properties must be ASKED FOR on the create, not just on the search. The CRM create
	// endpoint returns only system fields otherwise, so the assigned token would come back empty
	// from every real call — and the struct assertion above would still pass against a
	// hand-written fixture. This half is what makes that assertion mean anything.
	for _, want := range []string{"hs_utm", "hs_name"} {
		if !strings.Contains(gotPath+gotQuery, want) {
			t.Errorf("create does not request %s back: path=%q query=%q", want, gotPath, gotQuery)
		}
	}
	// We must NOT send hs_utm. HubSpot assigns it, and sending one would either be ignored or
	// set a token this service invented.
	if strings.Contains(gotBody, "hs_utm") {
		t.Errorf("create sent hs_utm, which HubSpot assigns: %s", gotBody)
	}
}

// An id-less 2xx means the campaign may or may not exist and cannot be addressed either way.
// Reporting success would hand back a reference that does not work; the error tells the caller
// to check HubSpot rather than retry blindly into a duplicate.
func TestCreateCampaign_IDLessResponseIsAnError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"properties":{"hs_name":"Nameless"}}`)
	})

	_, err := c.CreateCampaign(context.Background(), "nameless")
	if err == nil {
		t.Fatal("an id-less create was reported as success")
	}
	// The message must tell the caller not to retry blindly — a duplicate in an LF-global
	// namespace is visible to every foundation.
	if !strings.Contains(err.Error(), "check HubSpot") {
		t.Errorf("error does not warn against a blind retry: %v", err)
	}
}

func TestCreateCampaign_RefusesAnEmptyNameWithoutCallingHubSpot(t *testing.T) {
	called := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"id":"1"}`)
	})

	for _, n := range []string{"", "   "} {
		if _, err := c.CreateCampaign(context.Background(), n); err == nil {
			t.Errorf("an empty name (%q) was sent", n)
		}
	}
	if called {
		t.Error("HubSpot was contacted for an empty name")
	}
}

// Capped comes from HubSpot's own `total`, not from len(results)==limit. The distinction is the
// whole point: an exactly-full page and a truncated one look identical by length, so inferring
// from length would warn on a complete result set and teach operators to ignore the warning.
func TestSearchCampaigns_CappedComesFromTotalNotLength(t *testing.T) {
	t.Run("more matched than returned", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"total":250,"results":[{"id":"1","properties":{"hs_name":"A"}}]}`)
		})
		got, err := c.SearchCampaigns(context.Background(), "kubecon")
		if err != nil {
			t.Fatalf("SearchCampaigns: %v", err)
		}
		if !got.Capped {
			t.Error("Capped = false while HubSpot reported 250 matches for 1 returned row")
		}
	})

	t.Run("every match returned", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"total":1,"results":[{"id":"1","properties":{"hs_name":"A"}}]}`)
		})
		got, err := c.SearchCampaigns(context.Background(), "kubecon")
		if err != nil {
			t.Fatalf("SearchCampaigns: %v", err)
		}
		if got.Capped {
			t.Error("Capped = true on a complete result set — every operator would see a warning that never clears")
		}
	})

	t.Run("absent total is capped, not complete", func(t *testing.T) {
		// An omitted total says nothing about how many matched. Decoded into a plain int it
		// would read as "0 matched, nothing hidden" — the authoritative absence a caller acts on
		// by creating a campaign in a namespace every foundation shares. Unknown must fail
		// closed, so the absence of the field is treated as possibly capped.
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[]}`)
		})
		got, err := c.SearchCampaigns(context.Background(), "nothing")
		if err != nil {
			t.Fatalf("SearchCampaigns: %v", err)
		}
		if !got.Capped {
			t.Error("Capped = false on a response with no total: an unknown completeness read as proven absence")
		}
	})

	t.Run("empty and complete", func(t *testing.T) {
		// The common case, and the one that must NOT warn: nothing matched, and nothing was
		// hidden. This is where the create offer is legitimate.
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"total":0,"results":[]}`)
		})
		got, err := c.SearchCampaigns(context.Background(), "nothing")
		if err != nil {
			t.Fatalf("SearchCampaigns: %v", err)
		}
		if got.Capped {
			t.Error("Capped = true on an empty, complete search — the create offer would never be available")
		}
	})
}

// The two predicates a create's caller branches on, asserted together because the pair is the
// contract: a definite rejection means retry is safe, an unconfirmed one means verify first.
func TestCreateOutcomePredicates(t *testing.T) {
	t.Run("a definite 4xx is a clean rejection", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"invalid name"}`)
		})
		_, err := c.CreateCampaign(context.Background(), "bad name")
		if err == nil {
			t.Fatal("a 400 was reported as success")
		}
		if !IsDefiniteRejection(err) {
			t.Error("a definite 400 is not reported as a clean rejection: the operator is sent to hunt for a campaign that was never created")
		}
		if IsUnconfirmed(err) {
			t.Error("a definite 400 reported as unconfirmed")
		}
	})

	t.Run("an undecodable 2xx is unconfirmed, not a rejection", func(t *testing.T) {
		// The POST already succeeded, so HubSpot has very likely created the campaign. Only our
		// reading of the body failed, and a caller told "nothing was created" would retry into
		// a duplicate in a namespace every foundation shares.
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":`)
		})
		_, err := c.CreateCampaign(context.Background(), "KubeCon NA 2027")
		if err == nil {
			t.Fatal("an undecodable body was reported as success")
		}
		if !IsUnconfirmed(err) {
			t.Error("an undecodable 2xx lost its unconfirmed signal: a caller will retry a create that already ran")
		}
		if IsDefiniteRejection(err) {
			t.Error("an undecodable 2xx reported as a clean rejection")
		}
	})

	t.Run("an id-less 2xx is unconfirmed", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"","properties":{}}`)
		})
		_, err := c.CreateCampaign(context.Background(), "KubeCon NA 2027")
		if err == nil {
			t.Fatal("an id-less 2xx was reported as success")
		}
		if !IsUnconfirmed(err) {
			t.Error("an id-less 2xx lost its unconfirmed signal")
		}
	})

	t.Run("an unclassifiable error is NOT a definite rejection", func(t *testing.T) {
		// Fail-closed: IsDefiniteRejection is asked positively precisely so an error this
		// package cannot classify is never reported as "nothing was created".
		if IsDefiniteRejection(errors.New("something else entirely")) {
			t.Error("an unrecognised error classified as a clean rejection — a create's outcome nobody established")
		}
	})
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The adoption read is one GetCampaignsByIds call, scoped to the CLIENT's account in both the
// body and the CustomerAccountId header — that scoping is the provenance check, since the
// Campaign object carries no account id to compare.
func TestGetCampaign_ReadsOneCampaignUnderTheClientsAccount(t *testing.T) {
	rec := &budgetRecorder{}
	var gotAcct atomic.Value
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAcct.Store(r.Header.Get("CustomerAccountId"))
		rec.record(r)
		_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"Name":"KubeCon EU — Search","Status":"BudgetPaused","CampaignType":"Search"}],"PartialErrors":[]}`)
	})
	ref, err := c.GetCampaign(context.Background(), "321")
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if ref == nil || ref.ID != "321" || ref.Name != "KubeCon EU — Search" || ref.Status != StatusBudgetPaused {
		t.Fatalf("ref = %+v", ref)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || !strings.HasSuffix(reqs[0].path, "/Campaigns/QueryByIds") {
		t.Fatalf("want exactly one POST .../Campaigns/QueryByIds, got %+v", reqs)
	}
	if want := `{"AccountId":1234567,"CampaignIds":[321],"CampaignType":"Search,Shopping,DynamicSearchAds,Audience,Hotel,PerformanceMax,App"}`; reqs[0].body != want {
		t.Errorf("body = %s, want %s", reqs[0].body, want)
	}
	if got, _ := gotAcct.Load().(string); got != "1234567" {
		t.Errorf("CustomerAccountId = %q, want the client's own account", got)
	}
}

// The read asks for EVERY campaign type, so a live non-Search campaign comes back and is refused
// DEFINITELY — never read as absent (a duplicate of a live campaign would follow) and never
// adopted into the Search slot.
func TestGetCampaign_NonSearchCampaignIsADefiniteRefusal(t *testing.T) {
	for _, typ := range []string{"Audience", "PerformanceMax", "Shopping", "DynamicSearchAds"} {
		t.Run(typ, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"Name":"n","Status":"Active","CampaignType":"`+typ+`"}]}`)
			})
			ref, err := c.GetCampaign(context.Background(), "321")
			if !errors.Is(err, ErrNotSearchCampaign) || ref != nil {
				t.Fatalf("got %+v, %v; want ErrNotSearchCampaign", ref, err)
			}
		})
	}
}

func TestGetCampaign_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		absent  bool
		wantErr bool
	}{
		{name: "every live status is adoptable", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","Status":"Suspended","CampaignType":"Search"}]}`},
		{name: "a deleted non-Search campaign is still absent", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","Status":"Deleted","CampaignType":"Audience"}]}`, absent: true},
		{name: "missing CampaignType is unverifiable, not assumed Search", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","Status":"Active"}]}`, wantErr: true},
		{name: "no such campaign, as a PartialError", status: 200, body: `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`, absent: true},
		// A campaign in ANOTHER account is answered exactly like a missing one: the read is
		// account-scoped, so this is the "wrong account" outcome.
		{name: "no such campaign in this account, as a fault", status: 400, body: `{"Errors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId"}]}`, absent: true},
		{name: "deleted is absent", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","Status":"Deleted","CampaignType":"Search"}]}`, absent: true},
		{name: "unknown status is unverifiable", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","Status":"Archived","CampaignType":"Search"}]}`, wantErr: true},
		{name: "missing status is unverifiable", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","CampaignType":"Search"}]}`, wantErr: true},
		{name: "non-string status is unverifiable", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"n","Status":7,"CampaignType":"Search"}]}`, wantErr: true},
		{name: "missing name is unverifiable", status: 200, body: `{"Campaigns":[{"Id":321,"Status":"Active","CampaignType":"Search"}]}`, wantErr: true},
		{name: "blank name is unverifiable", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"  ","Status":"Active","CampaignType":"Search"}]}`, wantErr: true},
		{name: "another campaign's slot is unverifiable", status: 200, body: `{"Campaigns":[{"Id":999,"Name":"n","Status":"Active","CampaignType":"Search"}]}`, wantErr: true},
		{name: "an unexplained null slot is unverifiable", status: 200, body: `{"Campaigns":[null]}`, wantErr: true},
		{name: "an omitted Campaigns field is unverifiable", status: 200, body: `{}`, wantErr: true},
		{name: "a malformed body is unverifiable", status: 200, body: `{"Campaigns":[`, wantErr: true},
		{name: "a duplicated Id is unverifiable", status: 200, body: `{"Campaigns":[{"Id":999,"Id":321,"Name":"n","Status":"Active","CampaignType":"Search"}]}`, wantErr: true},
		{name: "a substituted name is unverifiable", status: 200, body: `{"Campaigns":[{"Id":321,"Name":"bad\uD800name","Status":"Active","CampaignType":"Search"}]}`, wantErr: true},
		{name: "another PartialError is unverifiable", status: 200, body: `{"Campaigns":[null],"PartialErrors":[{"Code":105,"ErrorCode":"InvalidCredentials","Index":0}]}`, wantErr: true},
		{name: "5xx is unverifiable", status: 503, body: `{}`, wantErr: true},
		{name: "a 401 is unverifiable, not absent", status: 401, body: `{"Errors":[{"Code":105,"ErrorCode":"AuthenticationTokenExpired"}]}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			ref, err := c.GetCampaign(context.Background(), "321")
			switch {
			case tc.wantErr:
				if err == nil || ref != nil {
					t.Fatalf("want an error and no ref, got %+v, %v", ref, err)
				}
			case tc.absent:
				if err != nil || ref != nil {
					t.Fatalf("want (nil, nil) — Microsoft answered that there is no such campaign — got %+v, %v", ref, err)
				}
			default:
				if err != nil || ref == nil {
					t.Fatalf("want a ref, got %+v, %v", ref, err)
				}
			}
		})
	}
}

// A throttle that never clears is an UNVERIFIED answer, not an absence: the read is retried and
// then reported as an error.
func TestGetCampaign_ExhaustedThrottleIsAnErrorNotAbsence(t *testing.T) {
	var calls int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ref, err := c.GetCampaign(context.Background(), "321")
	if err == nil || ref != nil {
		t.Fatalf("want an error, got %+v, %v", ref, err)
	}
	if n := atomic.LoadInt32(&calls); n != retryMax+1 {
		t.Errorf("the idempotent read was sent %d times, want %d (one try plus every retry)", n, retryMax+1)
	}
}

func TestGetCampaign_MalformedIDRefusedBeforeAnyRequest(t *testing.T) {
	var calls int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, id := range []string{"", "0", "007", "-1", "1.5", "1e3", "abc", " 321", "321 ", "9223372036854775808"} {
		ref, err := c.GetCampaign(context.Background(), id)
		if !errors.Is(err, ErrNotACampaignID) || ref != nil {
			t.Errorf("GetCampaign(%q) = %+v, %v; want ErrNotACampaignID", id, ref, err)
		}
		if verr := ValidateCampaignID(id); !errors.Is(verr, ErrNotACampaignID) {
			t.Errorf("ValidateCampaignID(%q) = %v, want ErrNotACampaignID", id, verr)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("a malformed id reached Microsoft %d time(s)", n)
	}
	if err := ValidateCampaignID("9223372036854775807"); err != nil {
		t.Errorf("the largest int64 is a valid id: %v", err)
	}
}

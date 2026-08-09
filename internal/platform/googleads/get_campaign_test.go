// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawLookupRow builds a row with a resource name the caller chooses, so a test can make
// the two identity fields disagree or malform one of them. lookupRow always derives the
// resource name from the id and so can never express those cases.
func rawLookupRow(resourceName, id, name, status string) json.RawMessage {
	return json.RawMessage(`{"campaign":{"resourceName":` + jsonQuote(resourceName) +
		`,"id":` + jsonQuote(id) + `,"name":` + jsonQuote(name) + `,"status":` + jsonQuote(status) + `}}`)
}

// TestGetCampaign_Live is the happy path, and pins that the details an operator is shown
// before confirming a binding come from the SERVER rather than from the request.
func TestGetCampaign_Live(t *testing.T) {
	srv, query := newLookupServer(t, []json.RawMessage{
		lookupRow("555", "LFX | Campaign | proj | evt", StatusPaused),
	})
	client := newAccountsTestClient(t, srv)

	ref, err := client.GetCampaign(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if ref == nil {
		t.Fatal("ref = nil, want the campaign: a live campaign reported as absent tells an operator the id they typed is wrong when it is right")
	}
	if ref.ID != "555" || ref.Name != "LFX | Campaign | proj | evt" || ref.Status != StatusPaused {
		t.Errorf("ref = %+v, want {555 LFX | Campaign | proj | evt PAUSED}", *ref)
	}

	// Both filters must be server-side. The id one unquoted: campaign.id is an int64
	// field, and quoting it makes this a string comparison against a numeric column.
	q := query()
	if !strings.Contains(q, "campaign.id = 555") {
		t.Errorf("query lacks the unquoted server-side id filter: %s", q)
	}
	if strings.Contains(q, "campaign.id = '555'") {
		t.Errorf("query compares the int64 campaign.id against a string literal: %s", q)
	}
	if !strings.Contains(q, "campaign.status != 'REMOVED'") {
		t.Errorf("query lacks the REMOVED exclusion: %s", q)
	}
	// The name is SELECTed, not merely returned by accident — it is the field an operator
	// reads to confirm they are binding to the campaign they meant.
	if !strings.Contains(q, "campaign.name") {
		t.Errorf("query does not select campaign.name, so no confirmable detail is returned: %s", q)
	}
}

// TestGetCampaign_AbsentIsNotAnError: no row means no such campaign, and that is a clean
// (nil, nil) — the same trustworthy absence FindCampaignByName reports.
func TestGetCampaign_AbsentIsNotAnError(t *testing.T) {
	srv, _ := newLookupServer(t, nil)
	client := newAccountsTestClient(t, srv)

	ref, err := client.GetCampaign(context.Background(), "555")
	if err != nil {
		t.Fatalf("an absent campaign is not an error: %v", err)
	}
	if ref != nil {
		t.Fatalf("ref = %+v, want nil", *ref)
	}
}

// TestGetCampaign_RemovedReadsAsAbsent: a tombstone is a real record that cannot be bound
// to, and "you cannot adopt this" is what the caller needs to hear either way. Note the
// server-side filter already excludes REMOVED — this covers the row arriving anyway,
// which is the case the client-side check exists for.
func TestGetCampaign_RemovedReadsAsAbsent(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		lookupRow("555", "tombstone", StatusRemoved),
	})
	client := newAccountsTestClient(t, srv)

	ref, err := client.GetCampaign(context.Background(), "555")
	if err != nil {
		t.Fatalf("a removed campaign is an absence, not an error: %v", err)
	}
	if ref != nil {
		t.Fatalf("ref = %+v, want nil: a REMOVED campaign must never be offered for adoption", *ref)
	}
}

// TestGetCampaign_RejectsUnusableIDs is the pre-flight guard. Every case here must fail
// BEFORE the network call: a value that is not a campaign id cannot be interpolated into
// a query, and "007" would otherwise match campaign 7 and be reported as a confusing
// filter-not-honoured conflict rather than as the malformed request it is.
func TestGetCampaign_RejectsUnusableIDs(t *testing.T) {
	for _, tc := range []struct{ name, id, why string }{
		{"empty", "", "no id at all"},
		{"zero", "0", "0 is not a campaign id even though it is digits"},
		{"negative", "-1", "int64 field, but never negative"},
		{"leading zero", "007", "same campaign as 7 server-side, a different string here"},
		{"non-numeric", "abc", "not an int64 at all"},
		{"padded", " 555 ", "padding makes it malformed, not campaign 555"},
		{"overflow", "9223372036854775808", "one past math.MaxInt64"},
		{"injection", "555 OR campaign.id > 0", "would widen the query to the whole account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, query := newLookupServer(t, []json.RawMessage{lookupRow("7", "seven", StatusEnabled)})
			client := newAccountsTestClient(t, srv)

			ref, err := client.GetCampaign(context.Background(), tc.id)
			if err == nil {
				t.Fatalf("GetCampaign(%q) = %+v, want an error: %s", tc.id, ref, tc.why)
			}
			if q := query(); q != "" {
				t.Errorf("GetCampaign(%q) reached the API: %s", tc.id, q)
			}
		})
	}
}

// TestGetCampaign_FilterNotHonouredIsNeverAnAbsence is 25a for the by-id path: a row for
// a DIFFERENT campaign proves the WHERE clause was not applied, which invalidates the
// whole response. Skipping the row would reduce that to zero matches — i.e. (nil, nil),
// the value a caller reads as "this id names nothing, go create one".
func TestGetCampaign_FilterNotHonouredIsNeverAnAbsence(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		lookupRow("999", "some other campaign", StatusEnabled),
	})
	client := newAccountsTestClient(t, srv)

	ref, err := client.GetCampaign(context.Background(), "555")
	if err == nil {
		t.Fatalf("GetCampaign = %+v, want an error: a row for campaign 999 means the id filter was not honoured", ref)
	}
	if !strings.Contains(err.Error(), "not honoured") {
		t.Errorf("error = %v, want it to name the unhonoured filter", err)
	}
}

// TestGetCampaign_UnverifiableRowsFailClosed sweeps the row-level identity checks through
// the by-id path. Each case is a row this client cannot establish the identity of, and
// none may be reported as an absence.
func TestGetCampaign_UnverifiableRowsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  json.RawMessage
		want string
	}{
		{
			// 25c: excluding REMOVED is not an allow-list. An omitted proto field decodes
			// to "", and UNSPECIFIED/UNKNOWN are real values Google returns.
			"unrecognised status",
			rawLookupRow("customers/1234567890/campaigns/555", "555", "x", "UNKNOWN"),
			"unrecognised status",
		},
		{
			"empty status",
			rawLookupRow("customers/1234567890/campaigns/555", "555", "x", ""),
			"unrecognised status",
		},
		{
			// 25d: another customer's campaign is not one this client may adopt, even
			// though the trailing segment reads as the right id.
			"cross-customer resource name",
			rawLookupRow("customers/9999999999/campaigns/555", "555", "x", StatusEnabled),
			"another customer",
		},
		{
			"malformed resource name",
			rawLookupRow("garbage/555", "555", "x", StatusEnabled),
			"malformed",
		},
		{
			"identity fields disagree",
			rawLookupRow("customers/1234567890/campaigns/777", "555", "x", StatusEnabled),
			"disagree",
		},
		{
			// The whitespace-only resource name: present-but-garbage, not absent. Trimming
			// it into "absent" would let the row be adopted on the id alone despite one of
			// its two identity fields being unusable.
			"whitespace-only resource name",
			rawLookupRow("   ", "555", "x", StatusEnabled),
			"malformed",
		},
		{
			"no usable id",
			rawLookupRow("", "", "x", StatusEnabled),
			"no usable id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{tc.row})
			client := newAccountsTestClient(t, srv)

			ref, err := client.GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: an unverifiable row must never read as an absence", ref)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestGetCampaign_DuplicateRowsMustAgree: GAQL can return one campaign on several rows,
// so identical duplicates are tolerated. Rows that DISAGREE are not — and the name counts,
// not just the id, because the name is what the operator confirms against.
func TestGetCampaign_DuplicateRowsMustAgree(t *testing.T) {
	t.Run("identical duplicates are one campaign", func(t *testing.T) {
		srv, _ := newLookupServer(t, []json.RawMessage{
			lookupRow("555", "same", StatusEnabled),
			lookupRow("555", "same", StatusEnabled),
		})
		client := newAccountsTestClient(t, srv)

		ref, err := client.GetCampaign(context.Background(), "555")
		if err != nil {
			t.Fatalf("duplicate rows for one campaign are not a conflict: %v", err)
		}
		if ref == nil || ref.ID != "555" {
			t.Fatalf("ref = %+v, want campaign 555", ref)
		}
	})

	t.Run("disagreeing duplicates are not trustworthy", func(t *testing.T) {
		srv, _ := newLookupServer(t, []json.RawMessage{
			lookupRow("555", "one name", StatusEnabled),
			lookupRow("555", "a different name", StatusEnabled),
		})
		client := newAccountsTestClient(t, srv)

		if ref, err := client.GetCampaign(context.Background(), "555"); err == nil {
			t.Fatalf("GetCampaign = %+v, want an error: the operator would confirm against a name picked arbitrarily", ref)
		}
	})
}

// TestGetCampaign_UndecodableRowIsNotAnAbsence: a 2xx body we cannot parse says nothing
// about whether the campaign exists.
func TestGetCampaign_UndecodableRowIsNotAnAbsence(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{json.RawMessage(`{"campaign":"not an object"}`)})
	client := newAccountsTestClient(t, srv)

	if ref, err := client.GetCampaign(context.Background(), "555"); err == nil {
		t.Fatalf("GetCampaign = %+v, want an error", ref)
	}
}

// TestGetCampaign_SearchFailurePropagates: a transport or API failure is not an absence.
func TestGetCampaign_SearchFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client := newAccountsTestClient(t, srv)

	if ref, err := client.GetCampaign(context.Background(), "555"); err == nil {
		t.Fatalf("GetCampaign = %+v, want the API failure to propagate", ref)
	}
}

// TestGetCampaign_RejectsInvalidAccount: a malformed customer id must fail before the
// network call, exactly as it does for the by-name lookup.
func TestGetCampaign_RejectsInvalidAccount(t *testing.T) {
	srv, query := newLookupServer(t, []json.RawMessage{lookupRow("555", "x", StatusEnabled)})
	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "123-456-7890"}, // dashes: not digits-only
		WithBaseURL(srv.URL),
		WithTokenURL(srv.URL+"/token"),
		WithAPIVersion("v23"),
	)

	if ref, err := client.GetCampaign(context.Background(), "555"); err == nil {
		t.Fatalf("GetCampaign = %+v, want an invalid customer id to fail the lookup", ref)
	}
	if q := query(); q != "" {
		t.Errorf("lookup queried the API despite an invalid customer id: %s", q)
	}
}

// TestCampaignRowIdentity_IsSharedByBothLookups is the reason the helper exists. Both
// entry points must reject the SAME unverifiable row; if one drifts lenient it will be
// the by-id path, which is the worse direction — a caller handing over an id is about to
// bind a brief and attach real spend.
func TestCampaignRowIdentity_IsSharedByBothLookups(t *testing.T) {
	// One row, unverifiable for a reason neither path may excuse: its two identity fields
	// name different campaigns.
	row := rawLookupRow("customers/1234567890/campaigns/777", "555", "shared", StatusEnabled)

	byName := func() error {
		srv, _ := newLookupServer(t, []json.RawMessage{row})
		_, err := newAccountsTestClient(t, srv).FindCampaignByName(context.Background(), "shared")
		return err
	}
	byID := func() error {
		srv, _ := newLookupServer(t, []json.RawMessage{row})
		_, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
		return err
	}

	nameErr, idErr := byName(), byID()
	if nameErr == nil {
		t.Error("FindCampaignByName accepted a row whose identity fields disagree")
	}
	if idErr == nil {
		t.Error("GetCampaign accepted a row whose identity fields disagree")
	}
}

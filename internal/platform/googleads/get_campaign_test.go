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

// TestGetCampaign_RemovedRowForAnotherCampaignIsNotAnAbsence pins the ORDER of the two
// row checks, which is the whole substance of the guard.
//
// A tombstone skip and an id-filter check are both correct; placing the skip first is
// not. A `continue` on the not-live verdict never reaches a check below it, so a REMOVED
// row for a DIFFERENT campaign would leave through the skip untested. A response
// containing only such rows honoured NEITHER predicate (the query asks for one id AND
// excludes REMOVED) yet would return (nil, nil): the trustworthy absence a caller acts on
// by creating a second campaign against the same budget. campaignRowIdentity now
// establishes identity before status precisely so the id check can sit above the skip.
func TestGetCampaign_RemovedRowForAnotherCampaignIsNotAnAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []json.RawMessage
	}{
		{
			// The id field names another campaign, and the row is a tombstone.
			"removed row names another campaign",
			[]json.RawMessage{lookupRow("999", "someone else's campaign", StatusRemoved)},
		},
		{
			// campaign.id absent, so the resource name is the only identity evidence —
			// the fallback path, which must police the filter just as the id field does.
			"removed row's resource name names another campaign",
			[]json.RawMessage{rawLookupRow("customers/1234567890/campaigns/999", "", "gone", StatusRemoved)},
		},
		{
			// The live match is FIRST, so a response cannot buy trust by leading with a
			// good row: one row proving the filter was ignored condemns the whole
			// response, exactly as a name mismatch does in FindCampaignByName.
			"a live match does not redeem the rest of the response",
			[]json.RawMessage{
				lookupRow("555", "the campaign", StatusEnabled),
				lookupRow("999", "someone else's campaign", StatusRemoved),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, tc.rows)
			client := newAccountsTestClient(t, srv)

			ref, err := client.GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: a REMOVED row for campaign 999 proves the id filter was not honoured, and must never be skipped into an absence", ref)
			}
			if !strings.Contains(err.Error(), "not honoured") {
				t.Errorf("error = %v, want it to name the unhonoured filter", err)
			}
		})
	}
}

// TestGetCampaign_NamelessRowIsNotConfirmable: the name is the field an operator reads to
// confirm the binding, so a row that cannot supply one cannot be confirmed. Campaign.name
// is required and always populated, so an empty one in a response that SELECTed it is a
// truncated answer — an error (costing a retry), never a ref (binding real spend to a
// campaign nobody can identify) and never an absence.
func TestGetCampaign_NamelessRowIsNotConfirmable(t *testing.T) {
	for _, tc := range []struct{ name, campaignName string }{
		{"omitted name", ""},
		{"whitespace-only name", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{
				lookupRow("555", tc.campaignName, StatusEnabled),
			})
			client := newAccountsTestClient(t, srv)

			ref, err := client.GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: an unnamed campaign cannot be confirmed by the operator binding to it", ref)
			}
			if !strings.Contains(err.Error(), "no usable name") {
				t.Errorf("error = %v, want it to name the missing name", err)
			}
		})
	}
}

// TestGetCampaign_MalformedTombstoneIsNotAnAbsence: identity is established before status,
// so a tombstone must still say who it is. Judging status first would grant the premise the
// tombstone skip rests on — "this is unadoptable however it arrived" is only true once we
// know WHICH campaign it is — and hand back a clean absence on evidence these checks exist
// to reject.
func TestGetCampaign_MalformedTombstoneIsNotAnAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  json.RawMessage
		want string
	}{
		{
			"cross-customer tombstone",
			rawLookupRow("customers/9999999999/campaigns/555", "555", "gone", StatusRemoved),
			"another customer",
		},
		{
			"malformed resource name on a tombstone",
			rawLookupRow("garbage/555", "555", "gone", StatusRemoved),
			"malformed",
		},
		{
			"tombstone whose identity fields disagree",
			rawLookupRow("customers/1234567890/campaigns/777", "555", "gone", StatusRemoved),
			"disagree",
		},
		{
			"tombstone with no usable identity at all",
			rawLookupRow("", "", "gone", StatusRemoved),
			"no usable id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{tc.row})
			client := newAccountsTestClient(t, srv)

			ref, err := client.GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: a tombstone this client cannot identify is not evidence that campaign 555 is absent", ref)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestGetCampaign_LiveAndRemovedInOneResponseIsUntrustworthy: one campaign cannot be both,
// so a response asserting both has contradicted itself — and the row a caller would act on
// is the live one, which is exactly the one that must not be trusted here. Both orders are
// covered because the rows arrive in no guaranteed order and a leading live row must not buy
// trust for what follows it.
func TestGetCampaign_LiveAndRemovedInOneResponseIsUntrustworthy(t *testing.T) {
	live := lookupRow("555", "the campaign", StatusEnabled)
	gone := lookupRow("555", "the campaign", StatusRemoved)

	for _, tc := range []struct {
		name string
		rows []json.RawMessage
	}{
		{"removed first", []json.RawMessage{gone, live}},
		{"live first", []json.RawMessage{live, gone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, tc.rows)
			client := newAccountsTestClient(t, srv)

			ref, err := client.GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: campaign 555 cannot be live and removed at once", ref)
			}
			if !strings.Contains(err.Error(), "both live and") {
				t.Errorf("error = %v, want it to name the contradiction", err)
			}
		})
	}

	// The contrast that keeps the rule narrow: tombstones ALONE remain a clean absence.
	// That is the campaign asked about, reported unadoptable, which is what a caller needs.
	t.Run("tombstones alone are still an absence", func(t *testing.T) {
		srv, _ := newLookupServer(t, []json.RawMessage{gone, gone})
		ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
		if err != nil {
			t.Fatalf("a removed campaign is an absence, not an error: %v", err)
		}
		if ref != nil {
			t.Fatalf("ref = %+v, want nil", *ref)
		}
	})
}

// TestGetCampaign_DisagreeingTombstonesAreNotAnAbsence extends the agreement rule to the
// rows that never reach the live-row comparison.
//
// A tombstone skip means its row leaves the loop early, so before this the duplicate-details
// check applied to live rows only: two REMOVED rows naming campaign 555 with two different
// names both set the removed flag and the call returned (nil, nil). But a response that
// answers ONE id with TWO campaigns has contradicted itself, and the absence derived from it
// is not the trustworthy absence a caller may act on by creating a second paid campaign — it
// is a broken response whose most convenient reading was taken at face value. The narrowing
// contrast lives in the sibling test above: IDENTICAL tombstone duplicates stay an absence,
// because GAQL genuinely can return one campaign on several rows.
func TestGetCampaign_DisagreeingTombstonesAreNotAnAbsence(t *testing.T) {
	rows := []json.RawMessage{
		lookupRow("555", "the campaign", StatusRemoved),
		lookupRow("555", "a different campaign", StatusRemoved),
	}
	srv, _ := newLookupServer(t, rows)

	ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
	if err == nil {
		t.Fatalf("GetCampaign = %+v, want an error: one id answered with two different campaigns", ref)
	}
	if !strings.Contains(err.Error(), "different details") {
		t.Errorf("error = %v, want it to name the disagreement", err)
	}

	// An empty name on a tombstone is NOT a defect — it is never surfaced, so the live-row
	// name guard deliberately does not apply here. Only a genuine disagreement fails.
	t.Run("an unnamed tombstone is still an absence", func(t *testing.T) {
		srv, _ := newLookupServer(t, []json.RawMessage{
			lookupRow("555", "", StatusRemoved),
			lookupRow("555", "", StatusRemoved),
		})
		ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
		if err != nil {
			t.Fatalf("an unnamed tombstone is an absence, not an error: %v", err)
		}
		if ref != nil {
			t.Fatalf("ref = %+v, want nil", *ref)
		}
	})
}

// TestGetCampaign_MalformedUTF8RowIsNotACampaign covers the substitution encoding/json makes
// without telling anyone.
//
// A JSON document must be UTF-8 (RFC 8259 s8.1) and encoding/json does not enforce it: it
// replaces each malformed byte with U+FFFD and returns no error. FindCampaignByName is immune
// by accident — it compares the decoded name against the name it asked for, so a substitution
// surfaces as a filter-not-honoured error. This path has no expected name to compare against.
// The name IS the answer, the thing an operator reads to confirm the id resolves to the
// campaign they meant, so a substituted name would be a successful CampaignRef naming a
// campaign that does not go by that name.
//
// The check is on the RAW bytes, not on hunting U+FFFD in the decoded string, because U+FFFD
// is a legal character in a campaign name and a name that genuinely contains one is not a
// defect.
func TestGetCampaign_MalformedUTF8RowIsNotACampaign(t *testing.T) {
	// Built by hand rather than through lookupRow: jsonQuote would encode the bad byte as a
	// valid � escape, which is precisely the substitution under test.
	bad := json.RawMessage("{\"campaign\":{\"resourceName\":\"customers/1234567890/campaigns/555\"," +
		"\"id\":\"555\",\"name\":\"bad\xffname\",\"status\":\"" + StatusEnabled + "\"}}")

	srv, _ := newLookupServer(t, []json.RawMessage{bad})
	ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
	if err == nil {
		t.Fatalf("GetCampaign = %+v, want an error: the row is not valid UTF-8 and its name "+
			"would be silently rewritten", ref)
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("error = %v, want it to name the encoding fault", err)
	}
}

// TestGetCampaign_NamesCampaignNameCannotHoldAreRefused holds the returned name to the bounds
// of the field it came out of.
//
// The sub-tests split into the two halves that matter, and the second half is the point. A
// value Campaign.name cannot hold did not come from a campaign, so a response carrying one has
// already gone wrong; but over-rejection here is NOT the conservative choice it resembles.
// Adoption targets campaigns this service never created and never sanitized, so a legal-but-
// unusual name — a TAB, a line separator, a zero-width joiner — really does exist upstream,
// and refusing one reports "cannot trust this" about a campaign sitting right there. Google
// Ads v23 prohibits NUL, LF and CR and caps the field at maxCampaignNameRunes CHARACTERS;
// everything else is legal, which is why the guard is three explicit runes rather than
// unicode.IsControl (which would reject TAB, and would MISS U+2028/U+2029 anyway).
func TestGetCampaign_NamesCampaignNameCannotHoldAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		campaig string
		wantErr string
	}{
		{"past the character ceiling", strings.Repeat("a", maxCampaignNameRunes+1), "past the"},
		{"a NUL", "the\x00campaign", "NUL, LF or CR"},
		{"a line feed", "the\ncampaign", "NUL, LF or CR"},
		{"a carriage return", "the\rcampaign", "NUL, LF or CR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{lookupRow("555", tc.campaig, StatusEnabled)})
			ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: Campaign.name cannot hold this value", ref)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	// The narrowing half. Each of these is a name Google will store, so each must adopt.
	for _, tc := range []struct {
		name    string
		campaig string
	}{
		{"exactly at the ceiling", strings.Repeat("a", maxCampaignNameRunes)},
		{"a tab", "the\tcampaign"},
		{"a line separator", "the\u2028campaign"},
		{"a zero-width joiner", "the\u200djoined campaign"},
		{"a replacement character the campaign really has", "the\ufffdcampaign"},
	} {
		t.Run(tc.name+" is adoptable", func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{lookupRow("555", tc.campaig, StatusEnabled)})
			ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
			if err != nil {
				t.Fatalf("GetCampaign: %v — this is a name Google Ads accepts, so refusing it "+
					"answers \"cannot trust this\" about a campaign that is really there", err)
			}
			if ref == nil || ref.Name != tc.campaig {
				t.Fatalf("ref = %+v, want the name returned verbatim", ref)
			}
		})
	}
}

// TestGetCampaign_UnpairedSurrogateEscapeIsNotACampaign covers the second way a name can be
// rewritten in transit — the one utf8.Valid cannot see.
//
// `"bad\uD800name"` is six ASCII bytes for the escape, so the document is perfectly valid UTF-8
// and the raw-bytes check above passes it. The substitution happens later, when encoding/json
// RESOLVES the escape: an unpaired surrogate is not a Unicode scalar value, so it decodes to
// U+FFFD, and — as with a malformed byte — returns no error. That lands in exactly the value
// TestGetCampaign_NamesCampaignNameCannotHoldAreRefused deliberately admits, since U+FFFD is a
// legal campaign name character, so nothing downstream could tell the two apart.
//
// The narrowing half matters as much as the refusing half. A well-formed PAIR is how every
// non-BMP character reaches Go through JSON, so rejecting escapes wholesale would refuse every
// campaign with an emoji in its name — a false absence dressed as caution (a campaign that is
// really there, reported as unadoptable). A DOUBLED backslash is likewise ordinary data: it is a
// literal backslash followed by the text uD800, not an escape at all.
func TestGetCampaign_UnpairedSurrogateEscapeIsNotACampaign(t *testing.T) {
	// Built by hand rather than through lookupRow: jsonQuote emits UTF-8 directly, and the
	// escape sequence IS what is under test.
	row := func(rawName string) json.RawMessage {
		return json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555",` +
			`"id":"555","name":"` + rawName + `","status":"` + StatusEnabled + `"}}`)
	}

	for name, rawName := range map[string]string{
		"a lone high surrogate":            `bad\uD800name`,
		"a lone low surrogate":             `bad\uDC00name`,
		"a high surrogate paired with BMP": `bad\uD800Aname`,
		"a high surrogate at the end":      `badname\uDBFF`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{row(rawName)})
			ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: json.Unmarshal resolves this escape "+
					"to U+FFFD without complaint, offering a name this campaign does not have", ref)
			}
			if !strings.Contains(err.Error(), "surrogate") {
				t.Errorf("error = %v, want it to name the escape an operator has to go looking for", err)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		rawName  string
		wantName string
	}{
		{"a well-formed surrogate pair", `the \uD83D\uDE00 campaign`, "the \U0001F600 campaign"},
		{"two pairs in a row", `\uD83D\uDE00\uD83D\uDE01`, "\U0001F600\U0001F601"},
		{"a pair with a BMP escape between other text", `a\u00e9b\uD83D\uDE00c`, "a\u00e9b\U0001F600c"},
		{"a doubled backslash before uD800", `a\\uD800b`, `a\uD800b`},
		{"an escape that is not \\u at all", `a\"quoted\" name`, `a"quoted" name`},
	} {
		t.Run(tc.name+" is adoptable", func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{row(tc.rawName)})
			ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
			if err != nil {
				t.Fatalf("GetCampaign: %v — this escape decodes to a name Google Ads stores, so "+
					"refusing it answers \"cannot trust this\" about a campaign that is really there", err)
			}
			if ref == nil || ref.Name != tc.wantName {
				t.Fatalf("ref = %+v, want the name %q", ref, tc.wantName)
			}
		})
	}
}

// TestGetCampaign_DuplicateKeyRowIsNotACampaign covers the one self-disagreement the
// decoder resolves instead of reporting.
//
// Every other identity guard in this package catches a row that contradicts itself across
// FIELDS. A repeated key contradicts itself inside ONE field, and RFC 8259 leaves the
// outcome undefined: encoding/json keeps the last value, so the row below decodes as
// campaign 555, agrees with its own resource name, and would be returned as a confirmed
// binding — while a reader following the equally-permitted first-wins convention gets 999.
// The row identifies two campaigns and the adoption it feeds spends real money on one of
// them, so the only safe reading is that there is no reading.
func TestGetCampaign_DuplicateKeyRowIsNotACampaign(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{
			"the id twice",
			`{"campaign":{"resourceName":"customers/1234567890/campaigns/555",` +
				`"id":"999","id":"555","name":"c","status":"` + StatusEnabled + `"}}`,
		},
		{
			"a key this client does not read",
			`{"campaign":{"resourceName":"customers/1234567890/campaigns/555",` +
				`"id":"555","name":"c","status":"` + StatusEnabled + `","advertisingChannelType":"SEARCH","advertisingChannelType":"DISPLAY"}}`,
		},
		{
			"the wrapper key twice",
			`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555",` +
				`"name":"c","status":"` + StatusEnabled + `"},"campaign":{"resourceName":` +
				`"customers/1234567890/campaigns/555","id":"555","name":"c","status":"` + StatusEnabled + `"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{json.RawMessage(tc.row)})
			ref, err := newAccountsTestClient(t, srv).GetCampaign(context.Background(), "555")
			if err == nil {
				t.Fatalf("GetCampaign = %+v, want an error: the row declares a key twice", ref)
			}
			if !strings.Contains(err.Error(), "same JSON key twice") {
				t.Errorf("error = %v, want it to name the duplicate key", err)
			}
		})
	}
}

// TestFindCampaignByName_DuplicateKeyRowIsNotAnAbsence is the same guard on the other
// lookup path, where the consequence is worse: FindCampaignByName's caller creates a new
// PAID campaign when told nothing matched, so a row that decodes to one of two identities
// must not be allowed to stand in for either.
func TestFindCampaignByName_DuplicateKeyRowIsNotAnAbsence(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555",` +
		`"id":"999","id":"555","name":"c","status":"` + StatusEnabled + `"}}`)

	srv, _ := newLookupServer(t, []json.RawMessage{row})
	id, err := newAccountsTestClient(t, srv).FindCampaignByName(context.Background(), "c")
	if err == nil {
		t.Fatalf("FindCampaignByName = %q, want an error: the row declares its id twice", id)
	}
	if id != "" {
		t.Errorf("id = %q, want empty alongside the error", id)
	}
	if !strings.Contains(err.Error(), "same JSON key twice") {
		t.Errorf("error = %v, want it to name the duplicate key", err)
	}
}

// TestHasDuplicateKeys pins the two ways the walker can be wrong in the dangerous
// direction — missing a duplicate — and the one way it can be wrong in the merely
// annoying direction: reporting a duplicate for repeated keys in SIBLING objects, or for
// repeated STRING VALUES, neither of which is a contradiction.
func TestHasDuplicateKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"a plain object", `{"a":1,"b":2}`, false},
		{"the same key twice", `{"a":1,"a":2}`, true},
		{"a duplicate nested one level down", `{"a":{"b":1,"b":2}}`, true},
		{"a duplicate inside an array element", `{"a":[{"b":1},{"c":1,"c":2}]}`, true},
		{"the same key in sibling objects", `{"a":{"x":1},"b":{"x":1}}`, false},
		{"a key repeated as a VALUE", `{"a":"b","b":"a"}`, false},
		{"a string value equal to a later key", `{"a":"z","z":1}`, false},
		{"an array of equal strings", `{"a":["x","x"]}`, false},
		{"malformed json defers to Unmarshal", `{"a":`, false},
		{"a bare scalar", `"x"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDuplicateKeys([]byte(tc.in)); got != tc.want {
				t.Errorf("hasDuplicateKeys(%s) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

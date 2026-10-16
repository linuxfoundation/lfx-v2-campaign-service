// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"testing"
)

func TestCampaignAdopted_ReadsOnlyTheExplicitMarker(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   bool
	}{
		{"marked", `{"customerId":"1112223333","adopted":true}`, true},
		{"linkedin adopter blob", `{"platform":"linkedin-ads","accountId":"5","adopted":true}`, true},
		{"explicit false", `{"adopted":false}`, false},
		{"created row", `{"customerId":"1112223333","adGroupId":"7"}`, false},
		// A legacy adopted row on a non-LinkedIn platform says so only in prose. Prose is not a
		// flag a client may branch on, so it reads as not recorded.
		{"legacy steps prose only", `{"steps":["Campaign adopted: 1 (already exists on account; no child entities recorded, so activation is done in the platform's own UI)"]}`, false},
		{"empty", ``, false},
		{"unreadable", `not json`, false},
		{"not an object", `[1,2]`, false},
		{"wrong type", `{"adopted":"true"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Campaign{Result: json.RawMessage(tc.result)}
			if got := CampaignAdopted(c); got != tc.want {
				t.Errorf("CampaignAdopted(%s) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
	if CampaignAdopted(nil) {
		t.Error("a nil campaign reads as adopted")
	}
}

func TestMarkResultAdopted_PreservesProvenanceAndSetsTheMarker(t *testing.T) {
	out, err := MarkResultAdopted(json.RawMessage(`{"customerId":"1112223333","steps":["a"],"adopted":false}`))
	if err != nil {
		t.Fatalf("MarkResultAdopted: %v", err)
	}
	var got struct {
		CustomerID string   `json:"customerId"`
		Steps      []string `json:"steps"`
		Adopted    bool     `json:"adopted"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	// The account provenance is what the mismatch guards read back; losing it would wave a
	// foreign campaign through every later toggle and metrics read.
	if got.CustomerID != "1112223333" || len(got.Steps) != 1 || !got.Adopted {
		t.Errorf("marked blob = %s, want provenance preserved and adopted:true", out)
	}
	if !CampaignAdopted(&Campaign{Result: out}) {
		t.Errorf("CampaignAdopted does not read what MarkResultAdopted writes: %s", out)
	}
}

func TestMarkResultAdopted_EmptyAndNullBecomeAMarkerOnlyBlob(t *testing.T) {
	for _, in := range []string{``, `  `, `null`} {
		out, err := MarkResultAdopted(json.RawMessage(in))
		if err != nil {
			t.Fatalf("MarkResultAdopted(%q): %v", in, err)
		}
		if string(out) != `{"adopted":true}` {
			t.Errorf("MarkResultAdopted(%q) = %s, want {\"adopted\":true}", in, out)
		}
	}
}

func TestMarkResultAdopted_RefusesANonObjectBlob(t *testing.T) {
	for _, in := range []string{`[1]`, `"x"`, `42`, `{broken`} {
		if out, err := MarkResultAdopted(json.RawMessage(in)); err == nil {
			t.Errorf("MarkResultAdopted(%q) = %s, want an error: overwriting it would discard provenance", in, out)
		}
	}
}

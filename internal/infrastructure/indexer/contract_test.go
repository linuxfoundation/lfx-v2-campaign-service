// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"encoding/json"
	"testing"
)

// TestSubjectMatchesPlatformConvention pins the subject namespace. The Query Service side
// subscribes to `lfx.index.<object_type>` — the same shape as the platform's existing
// committee_document / project_document / individual_vote / vote_response subjects. A typo
// here publishes into the void: NATS core has no subscriber-required error, so the write
// succeeds and the resource simply never appears in search.
func TestSubjectMatchesPlatformConvention(t *testing.T) {
	if got, want := Subject(ObjectTypeBrief), "lfx.index.campaign_brief"; got != want {
		t.Errorf("Subject(brief) = %q, want %q", got, want)
	}
	if got, want := Subject(ObjectTypeCampaign), "lfx.index.campaign"; got != want {
		t.Errorf("Subject(campaign) = %q, want %q", got, want)
	}
}

// TestNewBodyMatchesQueryServiceContract pins every field against
// lfx-v2-query-service's TransactionBodyStub. The JSON tags are the actual wire contract —
// the Query Service decodes `_source` into that struct and its searcher reads `object_type`
// and `public` directly, so a renamed tag yields a document that indexes cleanly and then
// matches nothing.
func TestNewBodyMatchesQueryServiceContract(t *testing.T) {
	b := NewBody(ObjectTypeBrief, "brief-1", "cncf", nil)

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"object_ref":             "campaign_brief:brief-1",
		"object_type":            "campaign_brief",
		"object_id":              "brief-1",
		"public":                 false,
		"access_check_object":    "project:cncf",
		"access_check_relation":  "campaign_manager",
		"history_check_object":   "project:cncf",
		"history_check_relation": "campaign_manager",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	// No stray fields: an unexpected key means the struct drifted from the contract.
	for k := range got {
		if _, ok := want[k]; !ok && k != "data" {
			t.Errorf("unexpected field %q in the index document", k)
		}
	}
}

// TestNewBodyIsNeverPublic is called out separately because it is a data-exposure guard, not a
// formatting one. Every resource this service owns is project-scoped and gated on
// campaign_manager; the Query Service's searcher applies `"term": {"public": true}` to serve
// anonymous callers, so publishing public=true would expose campaign data to unauthenticated
// search.
func TestNewBodyIsNeverPublic(t *testing.T) {
	for _, ot := range []string{ObjectTypeBrief, ObjectTypeCampaign} {
		if NewBody(ot, "id", "proj", nil).Public {
			t.Errorf("%s documents must never be public — they are project-scoped", ot)
		}
	}
}

// TestNewBodyUsesTheGatingRelation pins the FGA values to what the deployed ruleset enforces
// (charts/.../ruleset.yaml: relation campaign_manager on project:{projectId}). Publishing a
// relation the authz model does not grant makes every document invisible to every user —
// indistinguishable, from the outside, from indexing being broken entirely.
func TestNewBodyUsesTheGatingRelation(t *testing.T) {
	b := NewBody(ObjectTypeCampaign, "c1", "linux-foundation", nil)
	if b.AccessCheckObject != "project:linux-foundation" {
		t.Errorf("access_check_object = %q, want project:linux-foundation", b.AccessCheckObject)
	}
	// This service has no read-only audience (architecture D2), so history and access share
	// the relation. If a marketing_auditor-style read role is ever added, this is the
	// assertion that should force the split to be deliberate.
	if b.HistoryCheckRelation != b.AccessCheckRelation {
		t.Errorf("history (%q) and access (%q) relations must match: D2 gives this service one relation for reads and writes",
			b.HistoryCheckRelation, b.AccessCheckRelation)
	}
}

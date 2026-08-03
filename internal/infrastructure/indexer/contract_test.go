// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubject(t *testing.T) {
	assert.Equal(t, "lfx.index.campaign_brief", Subject(ObjectTypeBrief))
	assert.Equal(t, "lfx.index.campaign", Subject(ObjectTypeCampaign))
}

// TestTransaction_MatchesTheIndexerEnvelope pins the wire shape against
// lfx-v2-indexer-service's LFXTransaction. Getting this wrong does not error anywhere in this
// service: the indexer REJECTS the message ("missing or invalid action in message data") and
// indexing silently does nothing, which is exactly what an earlier flat-body version did.
func TestTransaction_MatchesTheIndexerEnvelope(t *testing.T) {
	raw, err := json.Marshal(NewTransaction(ActionCreated, ObjectTypeBrief, "b1", "cncf", "Bearer t0ken", map[string]any{"id": "b1"}))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))

	// The four top-level fields the indexer requires.
	// V2 accepts ONLY past tense — its validator rejects "create"/"update"/"delete".
	assert.Equal(t, "created", got["action"], "a message with an invalid action is rejected outright")
	require.Contains(t, got, "headers", "headers are read from the PAYLOAD, not native NATS headers")
	require.Contains(t, got, "data")
	require.Contains(t, got, "indexing_config", "without it the resource has no id and no FGA metadata")

	// object_type must NOT be in the payload: the indexer derives it from the SUBJECT.
	assert.NotContains(t, got, "object_type",
		"the object type travels in the subject; a payload copy is ignored")

	cfg := got["indexing_config"].(map[string]any)
	assert.Equal(t, "b1", cfg["object_id"])
	assert.Equal(t, false, cfg["public"], "every resource here is project-scoped")
	// D2: one relation, on the owning project, for both access and history.
	assert.Equal(t, "project:cncf", cfg["access_check_object"])
	assert.Equal(t, "campaign_manager", cfg["access_check_relation"])
	assert.Equal(t, "project:cncf", cfg["history_check_object"])
	assert.Equal(t, "campaign_manager", cfg["history_check_relation"])
	assert.Equal(t, []any{"project:cncf"}, cfg["parent_refs"])
}

// TestTransaction_RoutesByObjectType pins that the object type reaches the SUBJECT even though
// it is not serialized into the payload.
func TestTransaction_RoutesByObjectType(t *testing.T) {
	tx := NewTransaction(ActionUpdated, ObjectTypeCampaign, "c1", "cncf", "Bearer t0ken", nil)
	assert.Equal(t, ObjectTypeCampaign, tx.ObjectType())
	assert.Equal(t, "lfx.index.campaign", Subject(tx.ObjectType()))

	raw, err := json.Marshal(tx)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	// Assert on the KEY, not a substring: "campaign" also occurs inside "campaign_manager".
	assert.NotContains(t, got, "object_type",
		"the type must not leak into the payload — it is subject-derived")
}

// TestTransaction_ActionsAreTheIndexersVocabulary guards the exact strings.
func TestTransaction_ActionsAreTheIndexersVocabulary(t *testing.T) {
	assert.Equal(t, "created", ActionCreated)
	assert.Equal(t, "updated", ActionUpdated)
	assert.Equal(t, "deleted", ActionDeleted)
}

// TestTransaction_CarriesTheAuthorizationHeader pins the header the indexer REQUIRES. Its
// validateV2Headers drops any V2 message whose `authorization` is missing or empty, so an empty
// map means the resource silently never gets indexed.
func TestTransaction_CarriesTheAuthorizationHeader(t *testing.T) {
	raw, err := json.Marshal(NewTransaction(ActionCreated, ObjectTypeBrief, "b1", "cncf", "Bearer t0ken", nil))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"authorization":"Bearer t0ken"`,
		"V2 REQUIRES a non-empty authorization header; a message without one is dropped")
}

// TestTransaction_DeleteCarriesTheBareID pins the delete payload. The indexer type-asserts
// delete data to a STRING and rejects an object with "expected string" — passing a document
// there means an archived resource is never removed from search.
func TestTransaction_DeleteCarriesTheBareID(t *testing.T) {
	tx := NewTransaction(ActionDeleted, ObjectTypeBrief, "b1", "cncf", "Bearer t0ken",
		BriefDoc{ID: "b1", ProjectID: "cncf"})
	assert.Equal(t, "b1", tx.Data, "a delete must carry the bare object id, not a document")

	// A create/update still carries the document.
	tx = NewTransaction(ActionUpdated, ObjectTypeBrief, "b1", "cncf", "Bearer t0ken",
		BriefDoc{ID: "b1", ProjectID: "cncf"})
	assert.IsType(t, BriefDoc{}, tx.Data)
}

// TestTransaction_CarriesSearchableNames pins the name metadata. The Query Service applies
// `name=` against top-level `name_and_aliases` and sorts on `sort_name` — a name nested inside
// `data` is never consulted — so leaving these empty makes a resource index cleanly and then be
// unfindable by name, which looks exactly like indexing being broken.
func TestTransaction_CarriesSearchableNames(t *testing.T) {
	tx := NewTransaction(ActionCreated, ObjectTypeBrief, "b1", "cncf", "Bearer t", nil,
		"kubecon-eu-2026", "KubeCon EU 2026")

	require.NotNil(t, tx.IndexingConfig)
	assert.Equal(t, "kubecon-eu-2026", tx.IndexingConfig.SortName, "the first name sorts")
	assert.Equal(t, []string{"kubecon-eu-2026", "KubeCon EU 2026"}, tx.IndexingConfig.NameAndAliases)

	// Blanks and duplicates are dropped rather than indexed as empty aliases.
	tx = NewTransaction(ActionCreated, ObjectTypeBrief, "b1", "cncf", "Bearer t", nil,
		"", "  ", "kubecon", "kubecon")
	assert.Equal(t, "kubecon", tx.IndexingConfig.SortName)
	assert.Equal(t, []string{"kubecon"}, tx.IndexingConfig.NameAndAliases)

	// A resource with no meaningful name omits both rather than emitting empties.
	tx = NewTransaction(ActionCreated, ObjectTypeBrief, "b1", "cncf", "Bearer t", nil)
	assert.Empty(t, tx.IndexingConfig.SortName)
	assert.Empty(t, tx.IndexingConfig.NameAndAliases)
}

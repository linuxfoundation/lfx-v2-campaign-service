// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package indexer publishes resource snapshots to NATS for the platform's Query Service,
// which consumes them into OpenSearch and serves the lists and revision history this service
// deliberately does not implement (architecture D5).
//
// The contract here is DERIVED from the platform, not invented:
//
//   - Subject `lfx.index.<object_type>` matches the four types already in use elsewhere
//     (committee_document, project_document, individual_vote, vote_response).
//   - The message body mirrors lfx-v2-query-service's TransactionBodyStub, whose own comment
//     marks it as the shape the indexed `_source` must have. The searcher reads `object_type`
//     and `public` directly out of `_source`, so those two must be right or a document indexes
//     successfully and then matches nothing.
//   - access_check_relation is `campaign_manager` on `project:<projectId>`, per architecture
//     D2 ("no new FGA object types; only relations on project") and the deployed
//     charts/.../ruleset.yaml, which gates every route on exactly that relation+object.
//
// D2 also explains why the HISTORY check equals the ACCESS check: this service has no
// read-only audience, so the same relation governs reads and writes.
package indexer

import "fmt"

// Object types this service indexes. Connections are deliberately absent — D5 excludes them
// (singleton per project, no listing consumer).
const (
	ObjectTypeBrief    = "campaign_brief"
	ObjectTypeCampaign = "campaign"
)

// fgaRelation is the single OpenFGA relation gating this service. Every endpoint uses it for
// both reads and writes (architecture D2), so access and history checks share it.
const fgaRelation = "campaign_manager"

// subjectPrefix is the platform's indexing subject namespace.
const subjectPrefix = "lfx.index."

// Subject returns the NATS subject for an object type.
func Subject(objectType string) string { return subjectPrefix + objectType }

// Body is the indexed document envelope. Field names and JSON tags mirror the Query Service's
// TransactionBodyStub exactly; changing one without the other silently breaks indexing.
type Body struct {
	ObjectRef            string `json:"object_ref"`
	ObjectType           string `json:"object_type"`
	ObjectID             string `json:"object_id"`
	Public               bool   `json:"public"`
	AccessCheckObject    string `json:"access_check_object"`
	AccessCheckRelation  string `json:"access_check_relation"`
	HistoryCheckObject   string `json:"history_check_object"`
	HistoryCheckRelation string `json:"history_check_relation"`
	Data                 any    `json:"data,omitempty"`
}

// NewBody builds an index document for a project-scoped resource.
//
// Public is always FALSE: every resource this service owns is scoped to a project and gated on
// campaign_manager, so a public document would be visible to anonymous callers — the searcher
// filters on that field directly (`"term": {"public": true}`), making a wrong value an
// immediate data-exposure bug rather than a cosmetic one.
func NewBody(objectType, objectID, projectID string, data any) Body {
	fgaObject := "project:" + projectID
	return Body{
		ObjectRef:            fmt.Sprintf("%s:%s", objectType, objectID),
		ObjectType:           objectType,
		ObjectID:             objectID,
		Public:               false,
		AccessCheckObject:    fgaObject,
		AccessCheckRelation:  fgaRelation,
		HistoryCheckObject:   fgaObject,
		HistoryCheckRelation: fgaRelation,
		Data:                 data,
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package indexer publishes resource snapshots to NATS for lfx-v2-indexer-service, which
// indexes them into OpenSearch so the Query Service can serve the lists and revision history
// this service deliberately does not implement (architecture D5).
//
// The message shape is taken from the INDEXER's own contract
// (lfx-v2-indexer-service: internal/domain/contracts/transaction.go, pkg/types), not inferred
// from the OpenSearch document shape.
//
// That distinction matters and cost a review round: lfx-v2-query-service's
// `TransactionBodyStub` is the `_source` shape the indexer PRODUCES after processing a message
// — NOT the shape a producer sends. An earlier version of this file published that flat body,
// which the indexer rejects before indexing ("missing or invalid action in message data"). The
// service looked fully wired and indexed nothing.
package indexer

// Object types this service indexes. The type becomes the last token of the NATS subject, and
// the indexer derives it FROM the subject rather than the payload — a service can only publish
// to subjects for its own resource types, which is how that boundary is enforced.
//
// Connections are deliberately absent: D5 excludes them (singleton per project, no listing
// consumer).
const (
	ObjectTypeBrief    = "campaign_brief"
	ObjectTypeCampaign = "campaign"
)

// fgaRelation is the single OpenFGA relation gating this service. Architecture D2 forbids new
// FGA object types, so both the access and history checks use this relation on the owning
// project — there is no read-only audience that would justify separate relations.
const fgaRelation = "campaign_manager"

// subjectPrefix is the indexer's subscription root (its NATS_INDEXING_SUBJECT defaults to
// "lfx.index.>").
const subjectPrefix = "lfx.index."

// Message actions the indexer accepts. It also accepts the past-tense spellings
// ("created"/"updated"/"deleted"); the imperative forms are the v2 path.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
)

// Subject returns the NATS subject for an object type.
func Subject(objectType string) string { return subjectPrefix + objectType }

// Transaction is the envelope the indexer consumes (its LFXTransaction).
//
// Every field below is required for a create/update to be indexed: a message with no `action`
// is REJECTED outright, and without `indexing_config` the resource carries no object id and no
// FGA metadata, so it can be neither authorized nor found.
type Transaction struct {
	// Action is create/update/delete.
	Action string `json:"action"`
	// Headers carries the authenticated-principal HTTP headers from the originating request.
	// The indexer reads them from the PAYLOAD, not from native NATS headers.
	Headers map[string]string `json:"headers"`
	// Data is the resource snapshot for create/update; a delete passes only the resource id.
	Data any `json:"data"`
	// IndexingConfig carries the object id plus the FGA and search metadata.
	IndexingConfig *IndexingConfig `json:"indexing_config,omitempty"`

	// objectType is NOT serialized: the indexer derives the object type from the SUBJECT, and
	// including it in the payload would be ignored at best. It is carried here so the publisher
	// can route the message without the caller passing the type twice.
	objectType string
}

// ObjectType returns the type this message routes to.
func (t Transaction) ObjectType() string { return t.objectType }

// objectID returns the resource id for logging, tolerating a nil config.
func (t Transaction) objectID() string {
	if t.IndexingConfig == nil {
		return ""
	}
	return t.IndexingConfig.ObjectID
}

// IndexingConfig is the indexer's per-resource indexing configuration (its types.IndexingConfig).
type IndexingConfig struct {
	// ObjectID is the resource's unique id (required).
	ObjectID string `json:"object_id"`
	// Public indicates public accessibility. Always FALSE here: every resource this service
	// owns is project-scoped and gated on campaign_manager, so a public document would be
	// visible to anonymous callers — a data-exposure bug, not a cosmetic one.
	Public *bool `json:"public,omitempty"`

	// FGA fields, required for access control. Access and history are identical (see
	// fgaRelation).
	AccessCheckObject    string `json:"access_check_object"`
	AccessCheckRelation  string `json:"access_check_relation"`
	HistoryCheckObject   string `json:"history_check_object"`
	HistoryCheckRelation string `json:"history_check_relation"`

	// ParentRefs lets the Query Service find a resource by its parents.
	ParentRefs []string `json:"parent_refs,omitempty"`
}

// NewTransaction builds an indexer message for one resource.
//
// projectID is the OWNING project: both FGA checks are `project:<projectID>` per D2, so a
// resource is visible to exactly the people who can manage its project.
func NewTransaction(action, objectType, objectID, projectID string, data any) Transaction {
	public := false
	object := "project:" + projectID
	return Transaction{
		objectType: objectType,
		Action:     action,
		// No request headers are propagated: these publishes happen AFTER the write commits,
		// often on a context detached for shutdown, so the originating principal is not
		// reliably available. The map is non-nil because the indexer type-asserts it.
		Headers: map[string]string{},
		Data:    data,
		IndexingConfig: &IndexingConfig{
			ObjectID:             objectID,
			Public:               &public,
			AccessCheckObject:    object,
			AccessCheckRelation:  fgaRelation,
			HistoryCheckObject:   object,
			HistoryCheckRelation: fgaRelation,
			ParentRefs:           []string{object},
		},
	}
}

// The goa service types (briefs.Brief / briefs.Campaign) carry NO json tags, so marshalling
// them directly emits Go field names — "ProjectID", "EventSlug" — instead of the snake_case
// the HTTP API uses. Such a document indexes cleanly and then matches nothing for any consumer
// filtering on the API's field names, which looks exactly like indexing being broken.
//
// These types therefore restate the indexed shape EXPLICITLY with tags. They are deliberately
// hand-written rather than reusing the generated types: the index projection is a contract with
// the Query Service, and it should change only when someone edits it here.

// BriefDoc is the indexed representation of a brief.
type BriefDoc struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	ProgramType string `json:"program_type"`
	EventSlug   string `json:"event_slug"`
	URL         string `json:"url,omitempty"`
	Status      string `json:"status"`
	Version     int64  `json:"version"`
}

// CampaignDoc is the indexed representation of a campaign.
type CampaignDoc struct {
	ID                 string `json:"id"`
	ProjectID          string `json:"project_id"`
	BriefID            string `json:"brief_id"`
	Platform           string `json:"platform"`
	PlatformCampaignID string `json:"platform_campaign_id,omitempty"`
	CampaignName       string `json:"campaign_name"`
	Status             string `json:"status"`
	Version            int64  `json:"version"`
}

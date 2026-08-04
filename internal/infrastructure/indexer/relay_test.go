// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOutbox records what the relay retired vs left pending.
type fakeOutbox struct {
	pending   []*model.OutboxMessage
	published []int64
	failed    []int64
}

func (f *fakeOutbox) PendingIndexMessages(context.Context, int) ([]*model.OutboxMessage, error) {
	return f.pending, nil
}
func (f *fakeOutbox) MarkIndexMessagePublished(_ context.Context, id int64) error {
	f.published = append(f.published, id)
	return nil
}
func (f *fakeOutbox) RecordIndexMessageFailure(_ context.Context, id int64, _ string) error {
	f.failed = append(f.failed, id)
	return nil
}

// capturingPublisher records what reached the wire.
type capturingPublisher struct {
	subjects []string
	payloads [][]byte
	err      error
}

func (c *capturingPublisher) PublishRaw(_ context.Context, subject, _ string, payload []byte) error {
	if c.err != nil {
		return c.err
	}
	c.subjects = append(c.subjects, subject)
	c.payloads = append(c.payloads, payload)
	return nil
}

func outboxMsg(id int64, objectType string) *model.OutboxMessage {
	raw, _ := json.Marshal(NewTransaction(ActionDeleted, objectType, "b1", "cncf", "", "b1", "slug"))
	return &model.OutboxMessage{ID: id, ObjectType: objectType, ObjectID: "b1", Payload: raw}
}

// TestRelay_StampsTheServiceCredential pins that the relay supplies the authorization header the
// indexer requires — outbox rows deliberately store NO token, because the table is retained for
// audit and a per-request JWT written there would persist as a live credential indefinitely.
func TestRelay_StampsTheServiceCredential(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)}}
	pub := &capturingPublisher{}

	NewRelay(out, pub, "Bearer service-token").drain(context.Background())

	require.Len(t, pub.payloads, 1)
	var msg map[string]any
	require.NoError(t, json.Unmarshal(pub.payloads[0], &msg))

	headers := msg["headers"].(map[string]any)
	assert.Equal(t, "Bearer service-token", headers["authorization"],
		"the indexer drops any message without a non-empty authorization header")

	// The STORED payload must not have carried a credential.
	var stored map[string]any
	require.NoError(t, json.Unmarshal(out.pending[0].Payload, &stored))
	assert.Empty(t, stored["headers"].(map[string]any)["authorization"],
		"an outbox row must never persist a token")

	// Everything else survives the stamp byte-for-byte.
	assert.Equal(t, "deleted", msg["action"])
	assert.Equal(t, "b1", msg["data"])
	assert.Contains(t, msg, "indexing_config")

	// Routing comes from the ROW, not the payload (objectType is never serialized).
	assert.Equal(t, "lfx.index.campaign_brief", pub.subjects[0])
	assert.Equal(t, []int64{1}, out.published)
}

// TestRelay_LeavesUnsentRowsPending pins the outbox's core guarantee: a publisher that did not
// send must NOT have its rows retired. A pod started with indexing disabled would otherwise
// silently drain every pending message as "delivered", permanently defeating recovery.
func TestRelay_LeavesUnsentRowsPending(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)}}

	NewRelay(out, &capturingPublisher{err: errors.New("no connection")}, "Bearer t").drain(context.Background())

	assert.Empty(t, out.published, "a message that was never sent must not be marked published")
	assert.Equal(t, []int64{1}, out.failed, "the attempt is recorded so a stuck row is visible")
}

// TestNoopPublishRaw_ReportsFailure pins the same guarantee at the publisher. Noop sends
// nothing, so reporting success would let the relay retire rows that never left the process.
func TestNoopPublishRaw_ReportsFailure(t *testing.T) {
	err := Noop{}.PublishRaw(context.Background(), "lfx.index.campaign_brief", "b1", []byte("{}"))
	require.Error(t, err, "a Noop publish must not be reported as delivered")
}

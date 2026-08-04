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
	drainErr  error
}

// DrainPendingIndexMessages mirrors the real repo's claim semantics: deliver is called for each
// claimed message, and ONLY what deliver confirms is retired. Modelling the retire/leave-pending
// split is the point — a fake that always retired would hide exactly the bug this table exists
// to prevent.
func (f *fakeOutbox) DrainPendingIndexMessages(
	ctx context.Context,
	_ int,
	deliver func(context.Context, *model.OutboxMessage) error,
) (int, error) {
	if f.drainErr != nil {
		return 0, f.drainErr
	}
	// Model the real claim query, both halves:
	//
	//   - A RETIRED row is no longer pending (else the fake replays forever and the relay's
	//     drain-while-progressing loop sees an endless backlog).
	//   - At most ONE row per (object_type, object_id) per pass — the predecessor check. This
	//     is what makes a multi-message backlog take multiple passes, so a fake that drained
	//     everything at once could not tell a looping relay from a single-pass one.
	published := 0
	claimedObject := map[string]bool{}
	var remaining []*model.OutboxMessage
	for _, m := range f.pending {
		key := m.ObjectType + "\x00" + m.ObjectID
		if claimedObject[key] {
			remaining = append(remaining, m) // blocked behind an older row for the same object
			continue
		}
		claimedObject[key] = true
		if ctx.Err() != nil {
			remaining = append(remaining, m)
			continue
		}
		if err := deliver(ctx, m); err != nil {
			f.failed = append(f.failed, m.ID)
			remaining = append(remaining, m) // still pending, for a later pass
			continue
		}
		f.published = append(f.published, m.ID)
		published++
	}
	f.pending = remaining
	return published, nil
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
	row := outboxMsg(1, ObjectTypeBrief)
	// Capture the STORED bytes before the drain: a published row stops being pending, as in the
	// real query, so reading them back off the fake afterwards would find nothing.
	storedPayload := append([]byte(nil), row.Payload...)
	out := &fakeOutbox{pending: []*model.OutboxMessage{row}}
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
	require.NoError(t, json.Unmarshal(storedPayload, &stored))
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

// TestRelay_WithoutACredentialLeavesRowsPending pins the highest-stakes guard in the relay.
//
// With no service token the stamp would write an EMPTY authorization header. NATS accepts that
// publish, so drain would retire the row as delivered — while the indexer drops every
// empty-auth message. The outbox would silently drain itself and the recovery it exists for
// would be permanently lost. Skipping the pass keeps rows pending until the token is set.
func TestRelay_WithoutACredentialLeavesRowsPending(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)}}
	pub := &capturingPublisher{}

	for _, token := range []string{"", "   "} {
		out.published, out.failed = nil, nil
		pub.payloads = nil

		NewRelay(out, pub, token).drain(context.Background())

		assert.Empty(t, pub.payloads, "nothing may be published without a credential (token %q)", token)
		assert.Empty(t, out.published, "and no row may be retired: the indexer would have dropped it")
	}
}

// TestRelay_DrainsABacklogInOneTick covers the interaction between the per-resource claim and
// the relay's cadence.
//
// A pass claims at most ONE row per resource — that is what keeps a resource's messages in
// order — so a brief with a queued create+update+delete needs three passes. If the relay waited
// a full relayInterval between them, a backlog that is ready NOW would take 45s to drain, which
// is far too slow for the recovery this table exists to provide. drain therefore keeps passing
// while it makes progress.
func TestRelay_DrainsABacklogInOneTick(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{
		outboxMsg(1, ObjectTypeBrief),
		outboxMsg(2, ObjectTypeBrief),
		outboxMsg(3, ObjectTypeBrief),
	}}
	pub := &capturingPublisher{}

	NewRelay(out, pub, "Bearer service-token").drain(context.Background())

	assert.Len(t, pub.payloads, 3, "a ready backlog must drain within one tick, not one row per 15s")
	assert.Equal(t, []int64{1, 2, 3}, out.published, "and in id order")
	assert.Empty(t, out.pending, "nothing may be left behind")
}

// TestRelay_StopsWhenAPassPublishesNothing pins the loop's exit conditions.
//
// A publisher that fails every time makes no progress, so the loop must stop rather than spin on
// the same rows. The rows stay PENDING for a later tick, which is the whole point of the table —
// a failed delivery must not be mistaken for a delivered one.
func TestRelay_StopsWhenAPassPublishesNothing(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{
		outboxMsg(1, ObjectTypeBrief),
		outboxMsg(2, ObjectTypeBrief),
	}}
	pub := &capturingPublisher{err: errors.New("broker down")}

	NewRelay(out, pub, "Bearer service-token").drain(context.Background())

	assert.Empty(t, out.published, "a failed publish must never retire a row")
	assert.Len(t, out.pending, 2, "the rows stay pending for a later pass")
	// ONE attempt, not two: both rows belong to the same brief, so row 2 is blocked behind the
	// failed row 1 by the predecessor check. That is the point — publishing past a failed
	// message would reorder that resource's history, which is exactly what the claim prevents.
	// It also bounds the work: a failed delivery does not spin the loop on its whole backlog.
	assert.Equal(t, []int64{1}, out.failed,
		"a failed row must BLOCK its successor, not be skipped past")
}

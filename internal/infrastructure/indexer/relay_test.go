// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

	pruneCalls int
	pruned     int64
	pruneErr   error
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

// PrunePublishedIndexMessages records the prune call so a test can assert housekeeping runs
// AFTER delivery and never instead of it.
func (f *fakeOutbox) PrunePublishedIndexMessages(_ context.Context, _ time.Duration, _ int) (int64, error) {
	f.pruneCalls++
	return f.pruned, f.pruneErr
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

// TestRelay_PrunesAfterDelivering pins the ordering and the independence of the two jobs.
//
// The outbox grows with EVERY brief and campaign mutation and nothing else ever deletes a row,
// so without pruning the table, its backups, and the vacuum workload grow until storage runs
// out. The partial pending index stays small either way, which is exactly why the growth would
// go unnoticed until it mattered.
//
// Delivery comes FIRST: housekeeping must never delay the recovery the table exists for.
func TestRelay_PrunesAfterDelivering(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)}}
	pub := &capturingPublisher{}

	NewRelay(out, pub, "Bearer service-token").drain(context.Background())

	assert.Len(t, pub.payloads, 1, "delivery still happens")
	assert.Equal(t, 1, out.pruneCalls, "each pass prunes exactly once")
}

// TestRelay_PruneFailureDoesNotAffectDelivery pins that housekeeping cannot break indexing. A
// prune error costs disk, never correctness, so it is logged and dropped.
func TestRelay_PruneFailureDoesNotAffectDelivery(t *testing.T) {
	out := &fakeOutbox{
		pending:  []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)},
		pruneErr: errors.New("prune blew up"),
	}
	pub := &capturingPublisher{}

	NewRelay(out, pub, "Bearer service-token").drain(context.Background())

	assert.Len(t, pub.payloads, 1, "a failing prune must not stop delivery")
	assert.Equal(t, []int64{1}, out.published, "and must not stop the row being retired")
}

// TestRelay_NoCredentialSkipsPruningToo pins that a relay with no service credential does
// nothing at all. It cannot deliver, so it must not quietly delete history either — that would
// trim the audit trail while the recovery it backs is disabled.
func TestRelay_NoCredentialSkipsPruningToo(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)}}

	NewRelay(out, &capturingPublisher{}, "").drain(context.Background())

	assert.Zero(t, out.pruneCalls, "an idle relay must not prune")
	assert.Len(t, out.pending, 1, "and must leave the row pending")
}

// fakeResolver answers slug_to_uid from a table and counts lookups.
type fakeResolver struct {
	uids  map[string]string
	err   error
	calls int
}

func (f *fakeResolver) ProjectUID(_ context.Context, slug string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.uids[slug], nil
}

const relayTestUID = "7cad5a8d-19d0-41a4-81a6-043453daf9ee"

func campaignRow(id int64, objectID, project string) *model.OutboxMessage {
	raw, _ := json.Marshal(NewTransaction(ActionUpdated, ObjectTypeCampaign, objectID, project, "", map[string]any{"id": objectID}, "Name"))
	return &model.OutboxMessage{ID: id, ObjectType: ObjectTypeCampaign, ObjectID: objectID, Payload: raw}
}

func publishedConfig(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var msg map[string]any
	require.NoError(t, json.Unmarshal(payload, &msg))
	return msg["indexing_config"].(map[string]any)
}

// #590: with a resolver, the relay keys the FGA checks on the project UID, keeps the slug in
// parent_refs beside it, and leaves every other field and the stored row as written.
func TestRelay_KeysFGAChecksOnTheProjectUID(t *testing.T) {
	row := campaignRow(1, "c1", "cncf")
	stored := append([]byte(nil), row.Payload...)
	out := &fakeOutbox{pending: []*model.OutboxMessage{row}}
	pub := &capturingPublisher{}
	res := &fakeResolver{uids: map[string]string{"cncf": relayTestUID}}

	NewRelay(out, pub, "Bearer t").WithProjectUIDResolver(res).drain(context.Background())

	require.Len(t, pub.payloads, 1)
	cfg := publishedConfig(t, pub.payloads[0])
	assert.Equal(t, "project:"+relayTestUID, cfg["access_check_object"])
	assert.Equal(t, "project:"+relayTestUID, cfg["history_check_object"])
	assert.Equal(t, []any{"project:" + relayTestUID, "project:cncf"}, cfg["parent_refs"])
	assert.Equal(t, "campaign_manager", cfg["access_check_relation"])
	assert.Equal(t, "c1", cfg["object_id"])
	assert.Equal(t, "Name", cfg["sort_name"])
	assert.Equal(t, "project:cncf", publishedConfig(t, stored)["access_check_object"], "the stored row is never rewritten")
	assert.Equal(t, string(row.Payload), string(stored))
}

// A failed lookup must not publish a slug-keyed document nobody can read: the row stays
// pending, with the failure recorded, for a later pass.
func TestRelay_UnresolvedProjectLeavesTheRowPending(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{campaignRow(1, "c1", "cncf")}}
	pub := &capturingPublisher{}

	NewRelay(out, pub, "Bearer t").WithProjectUIDResolver(&fakeResolver{err: errors.New("unavailable")}).drain(context.Background())

	assert.Empty(t, pub.payloads)
	assert.Empty(t, out.published)
	assert.Equal(t, []int64{1}, out.failed)
	assert.Len(t, out.pending, 1)
}

// A message already keyed on a UID is published as written, without a lookup; a resolved slug
// is looked up once for every row that carries it.
func TestRelay_ResolvesEachSlugOnceAndSkipsUIDs(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{
		campaignRow(1, "c1", "cncf"),
		campaignRow(2, "c2", "cncf"),
		campaignRow(3, "c3", relayTestUID),
	}}
	pub := &capturingPublisher{}
	res := &fakeResolver{uids: map[string]string{"cncf": relayTestUID}}

	NewRelay(out, pub, "Bearer t").WithProjectUIDResolver(res).drain(context.Background())

	require.Len(t, pub.payloads, 3)
	assert.Equal(t, 1, res.calls, "one lookup per slug, none for a UID")
	assert.Equal(t, []any{"project:" + relayTestUID}, publishedConfig(t, pub.payloads[2])["parent_refs"])
}

// Without a resolver (the flag off) the message keeps the slug it was written with.
func TestRelay_WithoutAResolverKeepsTheSlug(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{campaignRow(1, "c1", "cncf")}}
	pub := &capturingPublisher{}

	NewRelay(out, pub, "Bearer t").drain(context.Background())

	require.Len(t, pub.payloads, 1)
	assert.Equal(t, "project:cncf", publishedConfig(t, pub.payloads[0])["access_check_object"])
}

// A delete is never held on the lookup: the indexer removes by object id, so an archive must
// reach it even when the project can no longer be resolved.
func TestRelay_DeletesPublishWithoutALookup(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{outboxMsg(1, ObjectTypeBrief)}}
	pub := &capturingPublisher{}
	res := &fakeResolver{err: errors.New("not found")}

	NewRelay(out, pub, "Bearer t").WithProjectUIDResolver(res).drain(context.Background())

	require.Len(t, pub.payloads, 1)
	assert.Equal(t, 0, res.calls)
	assert.Equal(t, []int64{1}, out.published)
}

// A failed lookup is remembered briefly, so every row of that project fails fast instead of
// each spending the resolver's timeout; rows of other projects still publish.
func TestRelay_RemembersAFailedLookup(t *testing.T) {
	out := &fakeOutbox{pending: []*model.OutboxMessage{
		campaignRow(1, "c1", "gone"),
		campaignRow(2, "c2", "gone"),
		campaignRow(3, "c3", "cncf"),
	}}
	pub := &capturingPublisher{}
	res := &failingFor{slug: "gone", uid: relayTestUID}

	NewRelay(out, pub, "Bearer t").WithProjectUIDResolver(res).drain(context.Background())

	assert.Equal(t, map[string]int{"gone": 1, "cncf": 1}, res.calls, "one lookup per slug, failures included")
	assert.Equal(t, []int64{3}, out.published)
	// The fake has no retry backoff, so the drain loop may try the failed rows again in a
	// later pass; the cached failure answers those attempts without another lookup.
	assert.Subset(t, []int64{1, 2}, out.failed)
	assert.Len(t, out.pending, 2)
}

type failingFor struct {
	slug  string
	uid   string
	calls map[string]int
}

func (f *failingFor) ProjectUID(_ context.Context, slug string) (string, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[slug]++
	if slug == f.slug {
		return "", errors.New("unavailable")
	}
	return f.uid, nil
}

// The resolver's answer must be a UID: an empty or non-UUID success leaves the row pending
// rather than keying the document on `project:` or on some other string.
func TestRelay_RefusesANonUIDAnswer(t *testing.T) {
	for _, answer := range []string{"", "cncf", "not-a-uuid"} {
		out := &fakeOutbox{pending: []*model.OutboxMessage{campaignRow(1, "c1", "cncf")}}
		pub := &capturingPublisher{}

		NewRelay(out, pub, "Bearer t").WithProjectUIDResolver(&fakeResolver{uids: map[string]string{"cncf": answer}}).drain(context.Background())

		assert.Empty(t, pub.payloads, "answer %q", answer)
		assert.Equal(t, []int64{1}, out.failed, "answer %q", answer)
	}
}

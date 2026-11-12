// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// OutboxReader is the outbox slice the relay needs. An interface so the relay is testable
// without a database.
//
// ONE method, not a read/mark/record triple. The claim, the publish and the retire have to
// happen under the same row locks — a relay that read rows, published, then marked them in
// separate calls let every replica load the SAME batch, so a slow pod could publish an earlier
// `updated` after a faster one had already published the later `deleted`. Handing the publish
// down as a callback is what keeps the lock held across it.
type OutboxReader interface {
	// DrainPendingIndexMessages claims a batch, calls deliver for each message, and retires
	// only what deliver confirms. A deliver error leaves that row pending for a later pass.
	DrainPendingIndexMessages(ctx context.Context, limit int, deliver func(context.Context, *model.OutboxMessage) error) (int, error)
	// PrunePublishedIndexMessages deletes PUBLISHED rows past the retention window. Pending
	// rows are undelivered work and are never eligible, however old.
	PrunePublishedIndexMessages(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

// RawPublisher publishes an already-marshalled payload to a subject. The relay must NOT
// re-marshal: the payload was fixed when the row was written, and re-deriving it would let a
// later contract change alter the meaning of a message enqueued under the old one.
type RawPublisher interface {
	PublishRaw(ctx context.Context, subject, objectID string, payload []byte) error
}

// ProjectUIDResolver resolves a project slug to its UID (lfx-v2-project-service's
// lfx.projects-api.slug_to_uid). accesscheck.Checker satisfies it.
type ProjectUIDResolver interface {
	ProjectUID(ctx context.Context, slug string) (string, error)
}

// projectUIDCacheTTL bounds how long a resolved UID is reused. A project's UID never changes,
// but its slug can be reassigned, so an entry is not kept forever.
const projectUIDCacheTTL = time.Hour

// projectUIDFailureTTL is how long a FAILED lookup is remembered. Every row of a project whose
// lookup just failed would otherwise repeat it, each costing up to the resolver's own timeout,
// and a few such rows would use up the whole pass. Short, so a transient outage is retried on
// the next pass or two.
const projectUIDFailureTTL = 30 * time.Second

// relayInterval is how often the relay drains the outbox. Frequent enough that a dropped
// publish is repaired in seconds rather than at the next write, cheap enough that the partial
// index makes an empty pass nearly free.
const relayInterval = 15 * time.Second

// relayPassTimeout bounds one drain so a slow broker or database cannot wedge the goroutine.
const relayPassTimeout = 30 * time.Second

// Relay drains the index outbox, publishing rows that the direct publish did not deliver.
//
// This is what makes indexing RECOVERABLE rather than best-effort: the outbox row co-commits
// with its resource, so a process that dies between commit and publish leaves the row behind
// and the relay picks it up. Without it a dropped message is simply lost — terminal writes like
// archiving a brief have no "next write" to repair the index.
type Relay struct {
	outbox OutboxReader
	pub    RawPublisher
	// authorization is the SERVICE credential injected at publish time. Outbox rows
	// deliberately store no token: the table is JSONB retained for audit with no pruning, so a
	// per-request JWT written there would persist as a live credential indefinitely.
	authorization string

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	// warnOnce keeps the missing-credential warning to a single line rather than one per pass.
	warnOnce sync.Once

	// projects, when set, keys each message's FGA checks on the project UID (#590). See
	// WithProjectUIDResolver.
	projects ProjectUIDResolver
	uidMu    sync.Mutex
	uidCache map[string]cachedUID
}

type cachedUID struct {
	uid     string
	err     error
	expires time.Time
}

// NewRelay constructs a Relay. authorization is the service credential stamped onto every
// replayed message; the indexer REQUIRES a non-empty authorization header and drops messages
// without one.
func NewRelay(outbox OutboxReader, pub RawPublisher, authorization string) *Relay {
	return &Relay{outbox: outbox, pub: pub, authorization: authorization}
}

// WithProjectUIDResolver makes the relay key every message's FGA checks on the project UID
// rather than the slug the outbox row was written with (#590).
//
// OpenFGA keys projects by UID, so a document whose access_check_object is `project:<slug>`
// matches no campaign_manager tuple and the Query Service filters it out for every reader. The
// gateway resolves the slug before its own openfga_check for the same reason (ruleset.yaml,
// project_slug_resolver_contextualizer). Resolving HERE, at publish time, rather than when the
// row is written means rows already pending (every row in an environment whose relay has been
// idle) are published correctly without being rewritten.
//
// access_check_object and history_check_object become `project:<uid>`. parent_refs carries
// BOTH `project:<uid>` and `project:<slug>`, so a reader filtering by either parent finds the
// document. A failed lookup fails the delivery, which leaves the row pending for a later pass:
// publishing it slug-keyed would index a document nobody can read. A DELETE is never held on
// the lookup: the indexer removes by object id, so its FGA fields decide nothing. The stored
// row is never changed. A nil resolver leaves messages as written.
//
// Accepted risk: the slug is resolved when the row is PUBLISHED, so if a slug moved to another
// project between the write and the publish, the document is keyed on the new project. That
// is no wider than the REST path, where the gateway resolves the path slug at request time
// and the service reads rows by the stored slug; the document is corrected by its next write.
func (r *Relay) WithProjectUIDResolver(res ProjectUIDResolver) *Relay {
	r.projects = res
	r.uidCache = map[string]cachedUID{}
	return r
}

// ResolvesProjectUID reports whether a project resolver is configured.
func (r *Relay) ResolvesProjectUID() bool { return r.projects != nil }

// projectUID resolves slug through the resolver, reusing a recent answer (or a recent failure).
func (r *Relay) projectUID(ctx context.Context, slug string) (string, error) {
	now := time.Now()
	r.uidMu.Lock()
	if c, ok := r.uidCache[slug]; ok && now.Before(c.expires) {
		r.uidMu.Unlock()
		return c.uid, c.err
	}
	r.uidMu.Unlock()
	uid, err := r.projects.ProjectUID(ctx, slug)
	if err == nil && !isUUID(uid) {
		// The resolver contract does not promise a UUID. Publishing `project:` or another
		// string would key the document on an object nobody (or the wrong project) holds.
		err = fmt.Errorf("resolver returned %q, not a UID", uid)
	}
	if err != nil {
		err = fmt.Errorf("indexer: resolve project %q to its UID: %w", slug, err)
		r.uidMu.Lock()
		r.uidCache[slug] = cachedUID{err: err, expires: now.Add(projectUIDFailureTTL)}
		r.uidMu.Unlock()
		return "", err
	}
	r.uidMu.Lock()
	r.uidCache[slug] = cachedUID{uid: uid, expires: now.Add(projectUIDCacheTTL)}
	r.uidMu.Unlock()
	return uid, nil
}

// keyOnProjectUID rewrites the FGA fields of a decoded message's indexing_config from
// `project:<slug>` to `project:<uid>` (see WithProjectUIDResolver). A message already keyed on
// a UID, or with no project-scoped access object, is left alone. Unknown indexing_config
// fields survive unchanged.
func (r *Relay) keyOnProjectUID(ctx context.Context, msg map[string]json.RawMessage) error {
	raw, ok := msg["indexing_config"]
	if !ok {
		return nil
	}
	var action string
	if a, ok := msg["action"]; ok && json.Unmarshal(a, &action) == nil && action == ActionDeleted {
		return nil
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("indexer: unparseable indexing_config: %w", err)
	}
	var access string
	if err := json.Unmarshal(cfg["access_check_object"], &access); err != nil {
		return nil
	}
	slug, ok := strings.CutPrefix(access, "project:")
	if !ok || slug == "" || isUUID(slug) {
		return nil
	}
	uid, err := r.projectUID(ctx, slug)
	if err != nil {
		return err
	}
	object := "project:" + uid
	var parents []string
	if pr, ok := cfg["parent_refs"]; ok {
		_ = json.Unmarshal(pr, &parents)
	}
	refs := []string{object}
	for _, p := range parents {
		if p != object {
			refs = append(refs, p)
		}
	}
	if !slices.Contains(refs, access) {
		refs = append(refs, access)
	}
	for key, value := range map[string]any{
		"access_check_object":  object,
		"history_check_object": object,
		"parent_refs":          refs,
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("indexer: encode %s: %w", key, err)
		}
		cfg[key] = encoded
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("indexer: encode indexing_config: %w", err)
	}
	msg["indexing_config"] = encoded
	return nil
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil && len(s) == 36
}

// Start begins draining in the background. Safe to call once; Stop ends it.
func (r *Relay) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(relayInterval)
		defer ticker.Stop()
		// Drain once at startup: the most likely reason rows are pending is that THIS pod's
		// predecessor died between a commit and its publish.
		r.drain(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.drain(ctx)
			}
		}
	}()
}

// Stop ends the relay and waits for the in-flight pass, bounded by wait.
//
// Bounded because this runs during shutdown: a slow broker must not hold the pod past its
// termination budget. An abandoned pass is safe — unpublished rows stay pending and the next
// process drains them, which is the entire point of the table.
func (r *Relay) Stop(wait time.Duration) {
	r.once.Do(func() {
		if r.cancel == nil {
			return
		}
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(wait):
			slog.Warn("index relay did not stop promptly; abandoning the pass (rows stay pending)")
		}
	})
}

// warnNoToken logs the missing-credential condition ONCE. Every pass would otherwise log it, and
// a message repeated every 15s trains operators to ignore it.
func (r *Relay) warnNoToken() {
	r.warnOnce.Do(func() {
		slog.Warn("index relay is idle: no service credential configured (set INDEXER_SERVICE_TOKEN). " +
			"Outbox rows stay PENDING and will be published once it is set — nothing is lost.")
	})
}

// stamp injects the service credential into a stored payload. The stored message deliberately
// carries no authorization (see Relay.authorization), and the indexer drops any message whose
// header is missing or empty. With a project resolver configured it also keys the message's FGA
// checks on the project UID (see WithProjectUIDResolver).
func (r *Relay) stamp(ctx context.Context, payload []byte) ([]byte, error) {
	// Decode into a MAP, not a Transaction. objectType is unexported and never serialized (the
	// indexer derives it from the subject), so a Transaction round-trip silently loses it — and
	// anything later reading ObjectType() off the result would route to "lfx.index.". The relay
	// routes from the outbox ROW instead, and a map keeps every other field byte-identical
	// including any the struct does not model.
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("indexer: unparseable outbox payload: %w", err)
	}
	headers := map[string]string{}
	if raw, ok := msg["headers"]; ok {
		// A malformed headers value is not fatal: replace it rather than dropping the message.
		_ = json.Unmarshal(raw, &headers)
	}
	headers[authorizationHeader] = r.authorization
	encoded, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("indexer: encode outbox headers: %w", err)
	}
	msg["headers"] = encoded
	if r.projects != nil {
		if err := r.keyOnProjectUID(ctx, msg); err != nil {
			return nil, err
		}
	}
	return json.Marshal(msg)
}

// drain publishes one batch of pending messages.
//
// The publish runs INSIDE the outbox's claim transaction (as the deliver callback), so the row
// locks are held across it and no other replica can process the same rows. That is what makes
// per-object ordering hold across replicas: the publisher's per-object lock is process-local and
// could never have provided it.
func (r *Relay) drain(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, relayPassTimeout)
	defer cancel()

	// Without a service credential every message would be REJECTED by the indexer for an empty
	// authorization header — but NATS would accept the publish, so the row would be retired as
	// delivered and the recovery this table exists for would be permanently lost. Skip the pass
	// instead: rows stay pending until the token is configured.
	if strings.TrimSpace(r.authorization) == "" {
		r.warnNoToken()
		return
	}

	// Loop while a pass makes progress. A pass claims at most ONE row per resource (that is
	// what keeps a resource's messages in order), so a brief with a queued create+update+delete
	// needs three passes. Waiting a full relayInterval between them would take 45s to drain a
	// backlog that is ready NOW — the recovery this table exists for should not be that slow.
	// Bounded by relayPassTimeout on the context and by a pass that publishes nothing.
	// maxPasses bounds the loop. A pass is only supposed to repeat while it drains a backlog,
	// but "published > 0" is progress as REPORTED by the outbox — a bug that published without
	// retiring would otherwise spin here forever, burning the broker on the same rows. The cap
	// turns that into a bounded pass and a resumed drain on the next tick.
	const maxPasses = 20
	total := 0
	for pass := 0; pass < maxPasses; pass++ {
		published, err := r.drainOnce(ctx)
		if err != nil {
			// Do not log a cancellation during shutdown as a failure.
			if parent.Err() == nil {
				slog.ErrorContext(ctx, "index relay could not drain the outbox", "error", err)
			}
			break
		}
		total += published
		// Nothing published means either an empty outbox or every remaining resource is blocked
		// behind a failed delivery. Either way another immediate pass would not help.
		if published == 0 {
			break
		}
		// Re-check the deadline: relayPassTimeout bounds the whole drain, not one pass.
		if ctx.Err() != nil {
			break
		}
	}
	if total > 0 {
		slog.InfoContext(ctx, "index relay published pending messages", "count", total)
	}
	r.prune(ctx, parent)
}

// prune trims published history so the outbox cannot grow without bound: every brief and
// campaign mutation writes a full JSONB payload, and nothing else ever deletes one.
//
// Runs after the drain, never before — delivery is the relay's job and must not queue behind
// housekeeping. A prune failure is logged and dropped: it costs disk, never correctness.
func (r *Relay) prune(ctx context.Context, parent context.Context) {
	if ctx.Err() != nil {
		return
	}
	deleted, err := r.outbox.PrunePublishedIndexMessages(ctx, 0, 0)
	if err != nil {
		if parent.Err() == nil {
			slog.ErrorContext(ctx, "index relay could not prune published outbox rows", "error", err)
		}
		return
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "index relay pruned published outbox rows", "count", deleted)
	}
}

// drainOnce runs a single claim-and-publish pass.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	return r.outbox.DrainPendingIndexMessages(ctx, 0, func(ctx context.Context, m *model.OutboxMessage) error {
		payload, aerr := r.stamp(ctx, m.Payload)
		if aerr != nil {
			// A row we cannot parse can never be published, and one whose project could not be
			// resolved must not be published slug-keyed. Returning the error records it on the
			// row and leaves it pending rather than retiring it.
			return aerr
		}
		return r.pub.PublishRaw(ctx, Subject(m.ObjectType), m.ObjectID, payload)
	})
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Server-sent progress for the email wizard.
//
// This is the ONE route in this service that is not a Goa method, and deliberately so: Goa
// v3 has no streaming-over-HTTP result for a plain GET, and expressing a long-lived
// text/event-stream response in the DSL would mean either generating a WebSocket endpoint
// the UI does not speak or returning one buffered blob at the end — which is the opposite of
// what progress is for. The handler is therefore hand-written net/http, mounted next to the
// generated routes in buildMux, and it re-implements exactly two things the generated code
// would have given it: bearer authentication (through the same authGuard every generated
// brief handler uses) and path decoding.
//
// The stream is a CONVENIENCE, never the channel of record. Every wizard turn also returns
// what it publishes here, so a client that cannot hold a stream open — a proxy that buffers,
// a browser that slept — recovers the same information from the turn's own response and from
// the session endpoint. That is what keeps this file free of retry, replay-buffer and
// at-least-once machinery.

const (
	// wizardProgressBuffer is the per-subscriber queue depth. Small on purpose: a wizard run
	// emits a handful of frames, and a subscriber that cannot keep up with five is not going
	// to be rescued by fifty.
	wizardProgressBuffer = 8
	// wizardHeartbeatInterval keeps intermediaries from reaping an idle stream. Content
	// generation regularly takes 30s or more with nothing to report, and many proxies drop a
	// silent connection well before that.
	wizardHeartbeatInterval = 15 * time.Second
	// wizardPollInterval is how often an attached stream re-reads the session row.
	//
	// The DB fallback exists because the publisher and the subscriber need not be the same
	// pod: with two replicas the turn runs on one and the stream is held by the other, and the
	// in-memory hub — which is per-process by design — would then deliver nothing at all. The
	// poll makes the stream eventually correct in every topology, which is why the hub can
	// stay a plain map instead of becoming a message broker.
	wizardPollInterval = 3 * time.Second
	// wizardStreamMaxDuration caps one stream. A wizard run is minutes; a connection still
	// open after this has been abandoned by a tab nobody is looking at, and the client
	// reconnects if it has not.
	wizardStreamMaxDuration = 30 * time.Minute
)

// WizardProgressFrame is one frame on the wizard's progress stream. Its JSON keys are the
// wire contract with the frontend's EventSource reader and must not be renamed.
type WizardProgressFrame struct {
	// Type is one of "brief" (an interim status line), "plan_done" (a turn finished, with
	// Result carrying that turn's own response body), "error" or "heartbeat".
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Result any    `json:"result,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Error  string `json:"error,omitempty"`
}

// WizardProgressHub fans frames out to the streams subscribed to a progress token.
//
// Per-process, with no persistence and no replay. A frame published while nobody is
// listening is DROPPED rather than queued: the alternative is an unbounded per-token buffer
// that a client which never connects would keep alive forever, and every frame's content is
// also returned by the turn that produced it.
type WizardProgressHub struct {
	mu   sync.Mutex
	subs map[string]map[chan WizardProgressFrame]struct{}
}

// NewWizardProgressHub returns an empty hub.
func NewWizardProgressHub() *WizardProgressHub {
	return &WizardProgressHub{subs: make(map[string]map[chan WizardProgressFrame]struct{})}
}

// Subscribe registers a stream for a token and returns it with its cancel function.
//
// The caller MUST call cancel (defer it), or the channel stays registered and every later
// publish on that token pays for a subscriber that will never read.
func (h *WizardProgressHub) Subscribe(token string) (<-chan WizardProgressFrame, func()) {
	ch := make(chan WizardProgressFrame, wizardProgressBuffer)
	token = strings.TrimSpace(token)
	if token == "" {
		// A closed channel rather than a nil one: the handler's select would block forever on
		// nil, and an empty token is a client mistake, not a reason to hold a goroutine.
		close(ch)
		return ch, func() {}
	}
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[string]map[chan WizardProgressFrame]struct{})
	}
	if h.subs[token] == nil {
		h.subs[token] = make(map[chan WizardProgressFrame]struct{})
	}
	h.subs[token][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if set := h.subs[token]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					// The token's map is removed with its last subscriber, so a long-lived
					// process does not accumulate one empty map per wizard session ever run.
					delete(h.subs, token)
				}
			}
			h.mu.Unlock()
			// Closed only here, and only once, and always with h.mu held by this function's
			// caller-side critical section above — Publish sends under the same mutex, so a
			// send can never race with this close.
			close(ch)
		})
	}
}

// Publish delivers a frame to every stream on a token.
//
// NON-BLOCKING, and that is the whole design. This is called from inside a request handler
// that is about to return a result to its own caller; making it wait on a browser's socket
// would let a stalled reader hold up the wizard turn itself. A subscriber whose buffer is
// full loses the frame and recovers from the DB poll or the turn's response.
func (h *WizardProgressHub) Publish(token string, frame WizardProgressFrame) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	// The send happens UNDER the lock, not after it. Copying the subscriber set and then
	// sending outside the lock left a window in which a concurrent `cancel()` — which runs in
	// the SSE handler's goroutine, while this runs in a wizard turn's — could close a channel
	// between the copy and the send. `send on closed channel` is an unrecoverable panic that
	// takes the whole pod down, not just the turn. Reproduced with 50 subscribers racing one
	// publish; the narrow single-subscriber case passes, which is why it was easy to miss.
	//
	// Holding the lock is safe precisely because every send is non-blocking: the `default`
	// arm means no subscriber can hold the mutex for longer than a buffered send, so a stalled
	// browser still cannot delay the wizard turn.
	h.mu.Lock()
	for ch := range h.subs[token] {
		select {
		case ch <- frame:
		default:
		}
	}
	h.mu.Unlock()
}

// Subscribers reports how many streams are attached to a token. For tests and for the
// handler's own logging; not a synchronization primitive.
func (h *WizardProgressHub) Subscribers(token string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[strings.TrimSpace(token)])
}

// WizardProgressHandler returns the hand-written SSE handler for the wizard's progress
// route, bound to the server's base context.
//
// baseCtx is the server's lifetime context, and taking it is what makes the stream
// shutdown-aware: http.Server.Shutdown waits for in-flight handlers without cancelling their
// request contexts, so a stream that watched only the request would keep a draining pod alive
// until the grace period expired on every attached browser.
func (s *BriefService) WizardProgressHandler(baseCtx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		projectID, briefID, token, ok := parseWizardProgressPath(r.URL.Path)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// The same guard the generated brief handlers authenticate through, so this route
		// cannot end up with a second, weaker answer to "is this token good?".
		//
		// Header only: no ?access_token= fallback, even though the browser EventSource API
		// cannot set headers. A credential in a query string is logged by every proxy in the
		// path and lands in browser history, and the UI already reads this stream with a
		// fetch-based reader that can send the header.
		bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		ctx, msg, unavailable := s.authenticate(r.Context(), bearer)
		if msg != "" {
			if unavailable {
				http.Error(w, msg, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("WWW-Authenticate", bearerChallenge)
			http.Error(w, msg, http.StatusUnauthorized)
			return
		}

		sessions, _, _ := s.wizardDeps()
		if sessions == nil {
			http.Error(w, "the email wizard is unavailable", http.StatusServiceUnavailable)
			return
		}
		// The token addresses the stream, but it does NOT authorize it: resolve the session it
		// belongs to and prove the path's project and brief are that session's own, or a caller
		// holding any valid LFX token could read another project's generated copy by guessing
		// (or being given) a token.
		sess, err := sessions.GetSessionByToken(ctx, token)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			slog.WarnContext(ctx, "could not resolve a wizard progress token",
				"error", safeErrSummary(err))
			http.Error(w, "the email wizard is unavailable", http.StatusServiceUnavailable)
			return
		}
		if sess.ProjectID != projectID || sess.BriefID != briefID {
			// 404, not 403: confirming that the token exists under some other project is
			// itself the leak.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		flusher, canFlush := w.(http.Flusher)
		if !canFlush {
			// Without flushing, every frame would sit in the response buffer until the stream
			// ended — which is indistinguishable from no progress at all, so say so instead of
			// pretending to stream.
			http.Error(w, "streaming is not supported by this server", http.StatusInternalServerError)
			return
		}

		// Subscribe BEFORE the first DB read, so a frame published between the two is queued
		// rather than lost in the gap.
		frames, unsubscribe := s.WizardProgress().Subscribe(token)
		defer unsubscribe()

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache, no-store")
		// Named explicitly because nginx buffers event-streams by default, which would hold
		// every frame until the response ended.
		h.Set("X-Accel-Buffering", "no")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// An immediate frame, so an attaching client renders something at once and — more
		// usefully — learns the phase of a turn that finished before it connected.
		lastPhase := string(sess.PhaseOrDefault())
		writeWizardFrame(w, flusher, WizardProgressFrame{
			Type: "brief",
			Text: "Connected to this email wizard session",
		})

		heartbeat := time.NewTicker(wizardHeartbeatInterval)
		defer heartbeat.Stop()
		poll := time.NewTicker(wizardPollInterval)
		defer poll.Stop()
		deadline := time.NewTimer(wizardStreamMaxDuration)
		defer deadline.Stop()

		for {
			select {
			case <-baseCtx.Done():
				// The pod is draining. Told rather than dropped, so the client reconnects to a
				// healthy replica instead of treating a closed socket as a failed run.
				writeWizardFrame(w, flusher, WizardProgressFrame{
					Type: "heartbeat", Text: "reconnecting", Done: true,
				})
				return
			case <-r.Context().Done():
				return
			case <-deadline.C:
				writeWizardFrame(w, flusher, WizardProgressFrame{
					Type: "heartbeat", Text: "reconnecting", Done: true,
				})
				return
			case frame, open := <-frames:
				if !open {
					return
				}
				if !writeWizardFrame(w, flusher, frame) {
					return
				}
				if frame.Done && frame.Type != "heartbeat" {
					// One turn per stream: the client opens a fresh stream (with a fresh token,
					// or the same one) for the next turn, so a finished run does not keep a
					// connection and a goroutine alive for the poll interval forever.
					return
				}
			case <-poll.C:
				// The cross-replica fallback. Only a PHASE CHANGE is reported, not the row's
				// contents: the turn's own response carries the payload, and re-sending a
				// generated email down the stream on every tick would make this poll expensive
				// in exactly the deployments that need it.
				fresh, perr := sessions.GetSessionByToken(ctx, token)
				if perr != nil {
					continue
				}
				if phase := string(fresh.PhaseOrDefault()); phase != lastPhase {
					lastPhase = phase
					if !writeWizardFrame(w, flusher, WizardProgressFrame{
						Type: "brief",
						Text: "This session is now at the " + phase + " step",
					}) {
						return
					}
				}
			case <-heartbeat.C:
				if !writeWizardFrame(w, flusher, WizardProgressFrame{Type: "heartbeat"}) {
					return
				}
			}
		}
	})
}

// writeWizardFrame writes one SSE event, reporting whether the stream is still usable.
func writeWizardFrame(w http.ResponseWriter, flusher http.Flusher, frame WizardProgressFrame) bool {
	payload, err := json.Marshal(frame)
	if err != nil {
		// A frame whose Result will not marshal is dropped rather than failing the stream: the
		// turn that produced it has already answered its own caller successfully.
		slog.Warn("could not encode a wizard progress frame", "type", frame.Type, "error", safeErrSummary(err))
		return true
	}
	if _, werr := w.Write([]byte("data: " + string(payload) + "\n\n")); werr != nil {
		// The client is gone. Not logged at warn: a closed tab is the normal end of a stream.
		return false
	}
	flusher.Flush()
	return true
}

// parseWizardProgressPath decodes /projects/{project_id}/briefs/{brief_id}/wizard/progress/{token}.
//
// Parsed here rather than read from the muxer's path variables so the handler depends on
// nothing but net/http, which is what lets a test exercise it with httptest alone. The shape
// is asserted rather than assumed: a mismatch is a 404, so a future route change cannot
// silently start serving streams for a path this function misreads.
func parseWizardProgressPath(path string) (projectID, briefID, token string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 7 {
		return "", "", "", false
	}
	if parts[0] != "projects" || parts[2] != "briefs" || parts[4] != "wizard" || parts[5] != "progress" {
		return "", "", "", false
	}
	projectID, briefID, token = parts[1], parts[3], parts[6]
	if projectID == "" || briefID == "" || token == "" {
		return "", "", "", false
	}
	return projectID, briefID, token, true
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestWizardProgressHub_DeliversToEverySubscriber — two browser tabs on the same run must
// both see the run's frames, which is why a token maps to a SET of channels rather than one.
func TestWizardProgressHub_DeliversToEverySubscriber(t *testing.T) {
	hub := NewWizardProgressHub()
	a, closeA := hub.Subscribe("tok-1")
	b, closeB := hub.Subscribe("tok-1")
	defer closeA()
	defer closeB()

	if got := hub.Subscribers("tok-1"); got != 2 {
		t.Fatalf("Subscribers = %d, want 2", got)
	}
	hub.Publish("tok-1", WizardProgressFrame{Type: "status", Text: "planning"})

	for name, ch := range map[string]<-chan WizardProgressFrame{"first": a, "second": b} {
		select {
		case frame := <-ch:
			if frame.Text != "planning" {
				t.Errorf("%s subscriber got %+v", name, frame)
			}
		default:
			t.Errorf("%s subscriber received nothing", name)
		}
	}
}

// TestWizardProgressHub_PublishNeverBlocks is the property that keeps a stalled browser from
// stopping the work. The publisher is the request goroutine doing the model call; if a full
// queue blocked it, one wedged reader would hang the wizard for that run.
func TestWizardProgressHub_PublishNeverBlocks(t *testing.T) {
	hub := NewWizardProgressHub()
	ch, unsubscribe := hub.Subscribe("tok-1")
	defer unsubscribe()

	// Far more frames than the queue holds, and nothing is reading.
	done := make(chan struct{})
	go func() {
		for i := 0; i < wizardProgressBuffer*10; i++ {
			hub.Publish("tok-1", WizardProgressFrame{Type: "status", Text: "tick"})
		}
		close(done)
	}()
	<-done // A blocking Publish would deadlock here rather than fail.

	if len(ch) != wizardProgressBuffer {
		t.Errorf("queue depth = %d, want the frames to be dropped at the bound %d", len(ch), wizardProgressBuffer)
	}
}

// TestWizardProgressHub_PublishToNobodyIsFine — the frames are advisory, and every wizard
// turn publishes whether or not a browser is attached.
func TestWizardProgressHub_PublishToNobodyIsFine(t *testing.T) {
	hub := NewWizardProgressHub()
	hub.Publish("tok-nobody", WizardProgressFrame{Type: "status", Text: "tick"})
	if got := hub.Subscribers("tok-nobody"); got != 0 {
		t.Errorf("Subscribers = %d, want 0", got)
	}
}

// TestWizardProgressHub_UnsubscribeIsIdempotent — the SSE handler unsubscribes from a defer
// and again on its exit paths, so a second call must not close a closed channel and panic
// the server.
func TestWizardProgressHub_UnsubscribeIsIdempotent(t *testing.T) {
	hub := NewWizardProgressHub()
	ch, unsubscribe := hub.Subscribe("tok-1")

	unsubscribe()
	unsubscribe()

	if _, open := <-ch; open {
		t.Error("unsubscribe must close the subscriber's channel")
	}
	if got := hub.Subscribers("tok-1"); got != 0 {
		t.Errorf("Subscribers after unsubscribe = %d, want 0 (the token's map must be reclaimed)", got)
	}
}

// TestWizardProgressHub_EmptyTokenSubscribesToNothing — an empty token would otherwise
// collect the frames of every run whose token failed to persist.
func TestWizardProgressHub_EmptyTokenSubscribesToNothing(t *testing.T) {
	hub := NewWizardProgressHub()
	ch, unsubscribe := hub.Subscribe("")
	defer unsubscribe()

	if _, open := <-ch; open {
		t.Error("an empty token must yield a closed channel, not a live subscription")
	}
	if got := hub.Subscribers(""); got != 0 {
		t.Errorf("Subscribers = %d, want 0", got)
	}
}

// TestParseWizardProgressPath pins the hand-written route's parsing. This handler is mounted
// outside Goa, so nothing generated validates the shape for it: a loose parse would let
// "/projects/p/briefs/b/wizard/progress" with a missing token resolve to an empty token, and
// an empty token must never look up a session.
func TestParseWizardProgressPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantOK  bool
		project string
		brief   string
		token   string
	}{
		{
			name: "well formed", path: "/projects/p1/briefs/b1/wizard/progress/tok-1",
			wantOK: true, project: "p1", brief: "b1", token: "tok-1",
		},
		{name: "missing token", path: "/projects/p1/briefs/b1/wizard/progress"},
		{name: "empty token segment", path: "/projects/p1/briefs/b1/wizard/progress/"},
		{name: "empty project", path: "/projects//briefs/b1/wizard/progress/tok-1"},
		{name: "empty brief", path: "/projects/p1/briefs//wizard/progress/tok-1"},
		{name: "extra segment", path: "/projects/p1/briefs/b1/wizard/progress/tok-1/extra"},
		{name: "wrong prefix", path: "/tenants/p1/briefs/b1/wizard/progress/tok-1"},
		{name: "wrong collection", path: "/projects/p1/drafts/b1/wizard/progress/tok-1"},
		{name: "wrong tail", path: "/projects/p1/briefs/b1/wizard/events/tok-1"},
		{name: "root", path: "/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project, brief, token, ok := parseWizardProgressPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if project != tc.project || brief != tc.brief || token != tc.token {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					project, brief, token, tc.project, tc.brief, tc.token)
			}
		})
	}
}

// progressHarness builds a wizard-enabled service with one session whose progress token is
// known, and returns the handler under test.
func progressHarness(t *testing.T) (http.Handler, *fakeWizardSessionRepo, string) {
	t.Helper()
	h := newWizardHarness(t, nil)
	h.svc.SetTokenVerifier(verifierFor("good-token", &model.Actor{Username: "someone", Email: "someone@example.org"}))
	started := h.start(t)
	return h.svc.WizardProgressHandler(context.Background()), h.sessions, h.sessions.items[started.SessionID].ProgressToken
}

// TestWizardProgressHandler_RefusalsBeforeStreaming covers every way this hand-written route
// must refuse. None of these may reach the stream: this handler is mounted outside Goa, so
// the auth middleware and the generated tenancy checks do not run for it.
func TestWizardProgressHandler_RefusalsBeforeStreaming(t *testing.T) {
	handler, _, token := progressHarness(t)

	tests := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{
			name: "no credential", want: http.StatusUnauthorized,
			path: "/projects/" + wizardTestProject + "/briefs/" + wizardTestBrief + "/wizard/progress/" + token,
		},
		{
			name: "refused credential", token: "bad-token", want: http.StatusUnauthorized,
			path: "/projects/" + wizardTestProject + "/briefs/" + wizardTestBrief + "/wizard/progress/" + token,
		},
		{
			name: "unknown token", token: "good-token", want: http.StatusNotFound,
			path: "/projects/" + wizardTestProject + "/briefs/" + wizardTestBrief + "/wizard/progress/no-such-token",
		},
		{
			// The leak this closes: a valid LFX token plus someone else's progress token.
			name: "token belongs to another project", token: "good-token", want: http.StatusNotFound,
			path: "/projects/other-project/briefs/" + wizardTestBrief + "/wizard/progress/" + token,
		},
		{
			name: "malformed path", token: "good-token", want: http.StatusNotFound,
			path: "/projects/" + wizardTestProject + "/briefs/" + wizardTestBrief + "/wizard/progress",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "text/event-stream") || strings.Contains(rec.Body.String(), "data:") {
				t.Error("a refused request must not have started streaming")
			}
		})
	}
}

// TestWizardProgressHandler_CredentialIsNeverAcceptedFromTheQueryString — EventSource cannot
// set headers, so the tempting fix is ?access_token=. A credential in a query string is
// logged by every proxy in the path; the UI uses a fetch reader instead, and this pins that
// the fallback was not quietly added back.
func TestWizardProgressHandler_CredentialIsNeverAcceptedFromTheQueryString(t *testing.T) {
	handler, _, token := progressHarness(t)

	path := "/projects/" + wizardTestProject + "/briefs/" + wizardTestBrief +
		"/wizard/progress/" + token + "?access_token=good-token"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a query-string token must not authenticate the stream", rec.Code)
	}
}

// TestWizardProgressHandler_StreamsThenEndsOnADoneFrame pins the streaming half: the headers
// that keep proxies from buffering, the immediate frame an attaching client renders, and the
// one-turn-per-stream exit.
func TestWizardProgressHandler_StreamsThenEndsOnADoneFrame(t *testing.T) {
	h := newWizardHarness(t, nil)
	h.svc.SetTokenVerifier(verifierFor("good-token", &model.Actor{Username: "someone", Email: "someone@example.org"}))
	started := h.start(t)
	token := h.sessions.items[started.SessionID].ProgressToken
	handler := h.svc.WizardProgressHandler(context.Background())

	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+
		"/projects/"+wizardTestProject+"/briefs/"+wizardTestBrief+"/wizard/progress/"+token, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer good-token")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no — nginx buffers event-streams by default", got)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	// The handler has subscribed by the time the headers arrive, so this frame cannot race.
	h.svc.WizardProgress().Publish(token, WizardProgressFrame{Type: "plan_done", Text: "finished", Done: true})

	buf := make([]byte, 4096)
	var body strings.Builder
	for {
		n, rerr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if rerr != nil {
			// A Done frame must END the stream, or a finished run keeps a goroutine and a
			// connection alive until the poll interval forever.
			break
		}
		if strings.Contains(body.String(), "finished") {
			// Keep reading until the server closes, which is the property under test.
			continue
		}
	}
	if !strings.Contains(body.String(), "Connected to this email wizard session") {
		t.Errorf("the stream must open with an immediate frame, got %q", body.String())
	}
	if !strings.Contains(body.String(), "finished") {
		t.Errorf("a published frame must reach the client, got %q", body.String())
	}
}

// TestWizardProgressHub_PublishDoesNotRaceUnsubscribe pins the fix for a panic that took the
// whole pod down, not just one turn.
//
// Publish used to copy the subscriber set under the mutex and then send OUTSIDE it. `cancel()`
// runs in the SSE handler's goroutine while Publish runs in a wizard turn's, so a close could
// land in that window: `send on closed channel`, which is unrecoverable.
//
// Fifty subscribers, because the single-subscriber case passes even against the broken code —
// the window is only wide enough to hit reliably when the send loop is long. Before the fix
// this panicked within a few iterations.
func TestWizardProgressHub_PublishDoesNotRaceUnsubscribe(t *testing.T) {
	for i := 0; i < 200; i++ {
		h := &WizardProgressHub{}
		cancels := make([]func(), 0, 50)
		for j := 0; j < 50; j++ {
			_, c := h.Subscribe("tok")
			cancels = append(cancels, c)
		}
		var wg sync.WaitGroup
		wg.Add(1 + len(cancels))
		go func() { defer wg.Done(); h.Publish("tok", WizardProgressFrame{}) }()
		for _, c := range cancels {
			go func(f func()) { defer wg.Done(); f() }(c)
		}
		wg.Wait()
	}
}

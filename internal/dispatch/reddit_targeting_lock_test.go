// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditStatefulStub is a Reddit API holding each ad group's targeting as state: a GET answers
// the current targeting, a PATCH replaces it with the one sent. A PATCH of an ad group listed in
// hold signals patched and then waits on release before it lands, so a test can park one write
// mid-flight and watch what a concurrent one does.
type redditStatefulStub struct {
	d *RedditDispatcher

	mu        sync.Mutex
	targeting map[string]json.RawMessage // ad group id -> targeting object
	gets      map[string]int
	patchN    map[string]int

	hold map[string]bool
	// delay marks ad groups whose PATCH COMMITS LATE: Reddit answers 502 (so the write is
	// UNCONFIRMED) and the targeting it carried is stashed in pending until commit lands it.
	delay   map[string]bool
	pending map[string]json.RawMessage
	// refuse answers an ad group's PATCH with this status, applying nothing (a definite refusal).
	refuse      map[string]int
	patched     chan string
	release     chan struct{}
	releaseOnce sync.Once
}

// releaseAll lets every parked PATCH land. Idempotent; also run at cleanup, so a failing test
// cannot leave a handler parked and hang the server's Close.
func (s *redditStatefulStub) releaseAll() { s.releaseOnce.Do(func() { close(s.release) }) }

const redditStubAdGroupsPrefix = "/api/v3/ad_accounts/t2_acct/ad_groups/"

func newRedditStatefulStub(t *testing.T, initial map[string]string, hold ...string) *redditStatefulStub {
	t.Helper()
	s := &redditStatefulStub{
		targeting: map[string]json.RawMessage{}, gets: map[string]int{}, patchN: map[string]int{},
		hold: map[string]bool{}, patched: make(chan string, 8), release: make(chan struct{}),
		delay: map[string]bool{}, pending: map[string]json.RawMessage{}, refuse: map[string]int{},
	}
	for id, obj := range initial {
		s.targeting[id] = json.RawMessage(obj)
	}
	for _, id := range hold {
		s.hold[id] = true
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, redditStubAdGroupsPrefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			s.gets[id]++
			tg := s.targeting[id]
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"id":"`+id+`","campaign_id":"t3_c","targeting":`+string(tg)+`}}`)
		case http.MethodPatch:
			var body struct {
				Data struct {
					Targeting json.RawMessage `json:"targeting"`
				} `json:"data"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil || len(body.Data.Targeting) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.mu.Lock()
			s.patchN[id]++
			held := s.hold[id]
			if st := s.refuse[id]; st != 0 {
				s.mu.Unlock()
				w.WriteHeader(st)
				_, _ = io.WriteString(w, `{"error":{}}`)
				return
			}
			if s.delay[id] {
				s.pending[id] = body.Data.Targeting
				s.mu.Unlock()
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{}`)
				return
			}
			s.mu.Unlock()
			if held {
				select {
				case s.patched <- id:
				default:
				}
				<-s.release
			}
			s.mu.Lock()
			s.targeting[id] = body.Data.Targeting
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"id":"`+id+`"}}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(api.Close)
	t.Cleanup(s.releaseAll) // runs before api.Close
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	s.d = NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	return s
}

// commit lands id's delayed PATCH, as Reddit finishing a request this service stopped waiting for.
func (s *redditStatefulStub) commit(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tg, ok := s.pending[id]; ok {
		s.targeting[id] = tg
		delete(s.pending, id)
	}
	s.delay[id] = false
}

func (s *redditStatefulStub) counts(id string) (gets, patches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[id], s.patchN[id]
}

func (s *redditStatefulStub) keywords(t *testing.T, id string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var tg map[string]json.RawMessage
	if err := json.Unmarshal(s.targeting[id], &tg); err != nil {
		t.Fatalf("stub targeting: %v", err)
	}
	return compactRaw(t, tg["keywords"])
}

// redditCampaignOn is redditBudgetCampaign recording adGroupID as its ad group.
func redditCampaignOn(adGroupID string) *model.Campaign {
	c := redditBudgetCampaign()
	c.Result = json.RawMessage(`{"accountId":"t2_acct","adGroupId":"` + adGroupID + `","adId":"t6_ad"}`)
	return c
}

func waitPatched(t *testing.T, s *redditStatefulStub, want string) {
	t.Helper()
	select {
	case got := <-s.patched:
		if got != want {
			t.Fatalf("PATCH parked on %s, want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first write never reached its PATCH")
	}
}

type removeResult struct {
	out []model.KeywordTargetingOutcome
	err error
}

func goRemove(s *redditStatefulStub, c *model.Campaign, rev string, keywords ...string) <-chan removeResult {
	ch := make(chan removeResult, 1)
	go func() {
		rm := make([]model.KeywordTargetingRemoval, 0, len(keywords))
		for _, k := range keywords {
			rm = append(rm, model.KeywordTargetingRemoval{Keyword: k})
		}
		out, err := s.d.RemoveKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, c, rm, rev)
		ch <- removeResult{out, err}
	}()
	return ch
}

// THE LOST UPDATE THIS FIX CLOSES. A removal and a negative add on one ad group, holding the same
// revision: the add must not read until the removal's PATCH has landed and been confirmed, so it
// then sees the removal's targeting, refuses as changed, and sends nothing — rather than passing
// the stale revision check and writing the removed keyword back.
func TestReddit_KeywordTargetingWrites_SameAdGroupSerialize(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	before := redditTargetingObject(`["kubernetes","jobs"]`)
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": before}, "t5_ag")
	rev := readRedditRevision(t, `["kubernetes","jobs"]`)

	removal := goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	waitPatched(t, s, "t5_ag")

	added := make(chan error, 1)
	go func() {
		_, err := s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"), []model.NegativeKeyword{broad("free")}, rev)
		added <- err
	}()
	// While the removal is parked in its PATCH, the add must be waiting — not reading.
	time.Sleep(150 * time.Millisecond)
	if gets, _ := s.counts("t5_ag"); gets != 1 {
		t.Fatalf("ad group read %d times while the removal held it; the add must wait for the lock", gets)
	}
	select {
	case err := <-added:
		t.Fatalf("the add finished while the removal held the lock: %v", err)
	default:
	}

	s.releaseAll()
	r := <-removal
	if r.err != nil || len(r.out) != 1 || r.out[0].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("removal = %+v, %v; want APPLIED", r.out, r.err)
	}
	err := <-added
	if !errors.Is(err, domain.ErrKeywordTargetingChanged) {
		t.Fatalf("add after the removal: err = %v, want ErrKeywordTargetingChanged (fresh read under the lock)", err)
	}
	if _, patches := s.counts("t5_ag"); patches != 1 {
		t.Errorf("%d PATCHes, want only the removal's", patches)
	}
	if got := s.keywords(t, "t5_ag"); got != `["kubernetes"]` {
		t.Errorf("keywords = %s, want the removal to stand", got)
	}
}

// A second write that cannot get the lock within its bounded wait is refused, definite and
// retryable, with nothing read or sent.
func TestReddit_KeywordTargetingWrites_SameAdGroupRefusesOnLockTimeout(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": redditTargetingObject(`["kubernetes","jobs"]`)}, "t5_ag")
	s.d.SetKeywordTargetingWriteLock(newLocalTargetingLock(50 * time.Millisecond))
	rev := readRedditRevision(t, `["kubernetes","jobs"]`)

	removal := goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	waitPatched(t, s, "t5_ag")
	defer func() { s.releaseAll(); <-removal }()

	_, err := s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"), []model.NegativeKeyword{broad("free")}, rev)
	if !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Fatalf("err = %v, want ErrKeywordTargetingWriteInProgress", err)
	}
	var unconfirmed interface{ Unconfirmed() bool }
	if errors.As(err, &unconfirmed) && unconfirmed.Unconfirmed() {
		t.Error("a lock refusal is definite, never UNCONFIRMED")
	}
	if gets, patches := s.counts("t5_ag"); gets != 1 || patches != 1 {
		t.Errorf("gets = %d, patches = %d after the refusal; it must read and send nothing", gets, patches)
	}
}

// Writes on DIFFERENT ad groups do not wait on each other.
func TestReddit_KeywordTargetingWrites_DifferentAdGroupsDoNotBlock(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	obj := redditTargetingObject(`["kubernetes","jobs"]`)
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": obj, "t5_ag2": obj}, "t5_ag")
	s.d.SetKeywordTargetingWriteLock(newLocalTargetingLock(50 * time.Millisecond))
	rev := readRedditRevision(t, `["kubernetes","jobs"]`)

	held := goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	waitPatched(t, s, "t5_ag")
	defer func() { s.releaseAll(); <-held }()

	select {
	case r := <-goRemove(s, redditCampaignOn("t5_ag2"), rev, "jobs"):
		if r.err != nil || len(r.out) != 1 || r.out[0].Outcome != model.KeywordOutcomeApplied {
			t.Fatalf("other ad group's removal = %+v, %v; want APPLIED while t5_ag is held", r.out, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a removal on another ad group waited on t5_ag's lock")
	}
	if got := s.keywords(t, "t5_ag2"); got != `["kubernetes"]` {
		t.Errorf("t5_ag2 keywords = %s", got)
	}
}

// recordingTargetingLock is a KeywordTargetingWriteLock fake that records the scope it was asked
// for, and when it was released relative to the stub's traffic.
type recordingTargetingLock struct {
	err      error
	scopes   []string
	released int
	cooled   []time.Duration
	onLock   func()
}

func (l *recordingTargetingLock) Lock(_ context.Context, scope string) (domain.KeywordTargetingLockHold, error) {
	l.scopes = append(l.scopes, scope)
	if l.err != nil {
		return nil, l.err
	}
	if l.onLock != nil {
		l.onLock()
	}
	return recordingHold{l}, nil
}

type recordingHold struct{ l *recordingTargetingLock }

func (h recordingHold) Release()                             { h.l.released++ }
func (h recordingHold) ReleaseAfterCooldown(d time.Duration) { h.l.cooled = append(h.l.cooled, d) }

// The lock is scoped to the account and the ad group, taken before the fresh read and released
// only after the confirming re-read; a lock that cannot be had sends nothing.
func TestReddit_KeywordTargetingWrites_LockScopeAndSpan(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": redditTargetingObject(`["kubernetes","jobs"]`)})
	lock := &recordingTargetingLock{}
	lock.onLock = func() {
		if gets, _ := s.counts("t5_ag"); gets != 0 {
			t.Errorf("the ad group was read %d times before the lock was taken", gets)
		}
	}
	s.d.SetKeywordTargetingWriteLock(lock)
	rev := readRedditRevision(t, `["kubernetes","jobs"]`)
	r := <-goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	if r.err != nil {
		t.Fatalf("removal: %v", r.err)
	}
	if len(lock.scopes) != 1 || lock.scopes[0] != "reddit/t2_acct/ad_groups/t5_ag" || lock.released != 1 {
		t.Fatalf("scopes = %v, released = %d", lock.scopes, lock.released)
	}
	if gets, patches := s.counts("t5_ag"); gets != 2 || patches != 1 {
		t.Errorf("gets = %d, patches = %d under one lock; want read, PATCH, re-read", gets, patches)
	}

	for _, refusal := range []error{domain.ErrKeywordTargetingWriteInProgress, errors.New("database unreachable")} {
		s2 := newRedditStatefulStub(t, map[string]string{"t5_ag": redditTargetingObject(`["kubernetes","jobs"]`)})
		s2.d.SetKeywordTargetingWriteLock(&recordingTargetingLock{err: refusal})
		_, err := s2.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"), []model.NegativeKeyword{broad("free")}, rev)
		if !errors.Is(err, refusal) || !domain.IsKeywordTargetingLockRefusal(err) {
			t.Errorf("err = %v, want %v, classed as a lock refusal (not a Reddit failure)", err, refusal)
		}
		if refusal != domain.ErrKeywordTargetingWriteInProgress && !errors.Is(err, domain.ErrKeywordTargetingLockUnavailable) {
			t.Errorf("a lock that could not be asked for must be ErrKeywordTargetingLockUnavailable: %v", err)
		}
		if gets, patches := s2.counts("t5_ag"); gets != 0 || patches != 0 {
			t.Errorf("%v: gets = %d, patches = %d; a refused lock must send nothing", refusal, gets, patches)
		}
	}
}

func TestLocalTargetingLock(t *testing.T) {
	l := newLocalTargetingLock(30 * time.Millisecond)
	ctx := context.Background()
	unlock, err := l.Lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Lock(ctx, "a"); !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Fatalf("second holder of a: %v, want ErrKeywordTargetingWriteInProgress", err)
	}
	unlockB, err := l.Lock(ctx, "b")
	if err != nil {
		t.Fatalf("another scope must not wait: %v", err)
	}
	unlockB.Release()

	// A waiter gets the lock once the holder releases it.
	got := make(chan error, 1)
	waiting := newLocalTargetingLock(5 * time.Second)
	u, _ := waiting.Lock(ctx, "a")
	go func() {
		u2, err := waiting.Lock(ctx, "a")
		if err == nil {
			u2.Release()
		}
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	u.Release()
	u.Release() // idempotent
	if err := <-got; err != nil {
		t.Fatalf("waiter: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.Lock(cctx, "a"); err == nil || errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Errorf("a cancelled wait = %v, want the context's error", err)
	}

	unlock.Release()
	l.mu.Lock()
	n := len(l.slots)
	l.mu.Unlock()
	waiting.mu.Lock()
	m := len(waiting.slots)
	waiting.mu.Unlock()
	if n != 0 || m != 0 {
		t.Errorf("slots left behind: %d, %d", n, m)
	}
}

// The time a write spends waiting for the lock is reported to the caller's lock-wait recorder, so
// the orchestrator can keep it out of Reddit's upstream latency.
func TestReddit_KeywordTargetingWrites_ReportLockWait(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": redditTargetingObject(`["kubernetes","jobs"]`)})
	const wait = 80 * time.Millisecond
	s.d.SetKeywordTargetingWriteLock(&recordingTargetingLock{onLock: func() { time.Sleep(wait) }})
	ctx, waited := domain.WithLockWaitRecorder(context.Background())
	rev := readRedditRevision(t, `["kubernetes","jobs"]`)
	if _, err := s.d.RemoveKeywordTargeting(ctx, "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"),
		[]model.KeywordTargetingRemoval{{Keyword: "jobs"}}, rev); err != nil {
		t.Fatalf("removal: %v", err)
	}
	if got := waited(); got < wait {
		t.Errorf("lock wait reported = %s, want at least %s", got, wait)
	}
}

// targetingWithExcluded is redditTargetingObject(keywords) with an excluded_keywords member.
func targetingWithExcluded(keywords, excluded string) string {
	return strings.Replace(redditTargetingObject(keywords), `"keywords":`, `"excluded_keywords":`+excluded+`,"keywords":`, 1)
}

func (s *redditStatefulStub) member(t *testing.T, id, name string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var tg map[string]json.RawMessage
	if err := json.Unmarshal(s.targeting[id], &tg); err != nil {
		t.Fatalf("stub targeting: %v", err)
	}
	return compactRaw(t, tg[name])
}

func revisionOfTargeting(t *testing.T, obj string) string {
	t.Helper()
	return redditRevisionOf(t, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":`+obj+`}}`)
}

// A DELAYED NEGATIVE ADD vs a waiting removal. The add's PATCH is UNCONFIRMED (502) and lands
// later; the batch also carries an ALREADY_PRESENT keyword, so the add answers per item with a nil
// error. Its lock must be kept for the cooldown: the removal waits, the delayed add lands inside
// the cooldown, and the removal then reads the changed targeting and refuses as changed — instead
// of removing "jobs", confirming, and having the late add put "jobs" back from its stale snapshot.
func TestReddit_UnconfirmedNegativeAddHoldsTheLockThroughTheCooldown(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	before := targetingWithExcluded(`["kubernetes","jobs"]`, `["spam"]`)
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": before})
	s.d.SetKeywordTargetingWriteLock(newLocalTargetingLock(5 * time.Second))
	const cooldown = 400 * time.Millisecond
	s.d.targetingCooldown = cooldown
	s.delay["t5_ag"] = true
	rev := revisionOfTargeting(t, before)

	out, err := s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"),
		[]model.NegativeKeyword{broad("spam"), broad("free")}, rev)
	if err != nil || len(out) != 2 || out[0].Outcome != model.KeywordOutcomeAlreadyPresent || out[1].Outcome != model.KeywordOutcomeUnconfirmed {
		t.Fatalf("add = %+v, %v; want [ALREADY_PRESENT, UNCONFIRMED] with a nil error", out, err)
	}
	gets, _ := s.counts("t5_ag")

	start := time.Now()
	removal := goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	time.Sleep(cooldown / 4)
	if g, _ := s.counts("t5_ag"); g != gets {
		t.Fatalf("the removal read the ad group %d time(s) during the unconfirmed add's cooldown", g-gets)
	}
	s.commit("t5_ag") // Reddit finishes the add late, inside the cooldown
	r := <-removal
	if waited := time.Since(start); waited < cooldown*3/4 {
		t.Errorf("the removal proceeded after %s, inside the %s cooldown", waited, cooldown)
	}
	if !errors.Is(r.err, domain.ErrKeywordTargetingChanged) {
		t.Fatalf("removal after the late add = %+v, %v; want ErrKeywordTargetingChanged", r.out, r.err)
	}
	if got := s.keywords(t, "t5_ag"); got != `["kubernetes","jobs"]` {
		t.Errorf("keywords = %s; nothing should have been removed on a stale revision", got)
	}
	if got := s.member(t, "t5_ag", "excluded_keywords"); got != `["spam","free"]` {
		t.Errorf("excluded_keywords = %s, want the late add to stand", got)
	}
}

// A DELAYED REMOVAL vs a waiting negative add: the symmetric case. The removal's PATCH is
// UNCONFIRMED and lands later; the add waits out the cooldown and then refuses as changed, instead
// of adding, confirming, and being erased by the late removal's stale snapshot.
func TestReddit_UnconfirmedRemovalHoldsTheLockThroughTheCooldown(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	before := redditTargetingObject(`["kubernetes","jobs"]`)
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": before})
	s.d.SetKeywordTargetingWriteLock(newLocalTargetingLock(5 * time.Second))
	const cooldown = 400 * time.Millisecond
	s.d.targetingCooldown = cooldown
	s.delay["t5_ag"] = true
	rev := revisionOfTargeting(t, before)

	r := <-goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	if !redditWriteUnsettled(r.err) {
		t.Fatalf("removal = %+v, %v; want UNCONFIRMED", r.out, r.err)
	}
	gets, _ := s.counts("t5_ag")

	start := time.Now()
	added := make(chan error, 1)
	go func() {
		_, err := s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"), []model.NegativeKeyword{broad("free")}, rev)
		added <- err
	}()
	time.Sleep(cooldown / 4)
	if g, _ := s.counts("t5_ag"); g != gets {
		t.Fatalf("the add read the ad group %d time(s) during the unconfirmed removal's cooldown", g-gets)
	}
	s.commit("t5_ag")
	err := <-added
	if waited := time.Since(start); waited < cooldown*3/4 {
		t.Errorf("the add proceeded after %s, inside the %s cooldown", waited, cooldown)
	}
	if !errors.Is(err, domain.ErrKeywordTargetingChanged) {
		t.Fatalf("add after the late removal: %v; want ErrKeywordTargetingChanged", err)
	}
	if got := s.keywords(t, "t5_ag"); got != `["kubernetes"]` {
		t.Errorf("keywords = %s, want the late removal to stand", got)
	}
}

// A cooldown longer than the waiter's own wait refuses the waiter as in progress, with nothing
// read or sent.
func TestReddit_CooldownLongerThanTheWaitRefusesTheWaiter(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	before := redditTargetingObject(`["kubernetes","jobs"]`)
	s := newRedditStatefulStub(t, map[string]string{"t5_ag": before})
	s.d.SetKeywordTargetingWriteLock(newLocalTargetingLock(100 * time.Millisecond))
	s.d.targetingCooldown = 5 * time.Second
	s.delay["t5_ag"] = true
	rev := revisionOfTargeting(t, before)

	if _, err := s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"), []model.NegativeKeyword{broad("free")}, rev); !redditWriteUnsettled(err) {
		t.Fatalf("add: %v; want UNCONFIRMED", err)
	}
	gets, patches := s.counts("t5_ag")
	r := <-goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
	if !errors.Is(r.err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Fatalf("removal during the cooldown: %v; want ErrKeywordTargetingWriteInProgress", r.err)
	}
	if g, p := s.counts("t5_ag"); g != gets || p != patches {
		t.Errorf("the refused removal sent %d read(s) and %d PATCH(es)", g-gets, p-patches)
	}
}

// Release is by OUTCOME: a settled write releases at once, an UNCONFIRMED one is held for the
// cooldown — on both paths, including the add's per-item UNCONFIRMED with a nil error.
func TestReddit_TargetingLockReleaseFollowsTheOutcome(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	before := targetingWithExcluded(`["kubernetes","jobs"]`, `["spam"]`)
	cases := []struct {
		name     string
		patch    int // 0: commit normally; >0: answer this status without applying
		delay    bool
		remove   bool
		keywords []model.NegativeKeyword
		cooled   bool
	}{
		{name: "removal applied", remove: true},
		{name: "removal refused by Reddit (definite 400)", remove: true, patch: http.StatusBadRequest},
		{name: "removal unconfirmed (502)", remove: true, delay: true, cooled: true},
		{name: "add applied", keywords: []model.NegativeKeyword{broad("free")}},
		{name: "add all already present (nothing sent)", keywords: []model.NegativeKeyword{broad("spam")}},
		{name: "add refused by Reddit (definite 400)", keywords: []model.NegativeKeyword{broad("free")}, patch: http.StatusBadRequest},
		{name: "add unconfirmed (502)", keywords: []model.NegativeKeyword{broad("free")}, delay: true, cooled: true},
		{name: "add mixed already present + unconfirmed", keywords: []model.NegativeKeyword{broad("spam"), broad("free")}, delay: true, cooled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditStatefulStub(t, map[string]string{"t5_ag": before})
			s.refuse["t5_ag"] = tc.patch
			lock := &recordingTargetingLock{}
			s.d.SetKeywordTargetingWriteLock(lock)
			s.delay["t5_ag"] = tc.delay
			rev := revisionOfTargeting(t, before)
			if tc.remove {
				<-goRemove(s, redditCampaignOn("t5_ag"), rev, "jobs")
			} else {
				_, _ = s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, redditCampaignOn("t5_ag"), tc.keywords, rev)
			}
			switch {
			case tc.cooled && (len(lock.cooled) != 1 || lock.cooled[0] != domain.KeywordTargetingUnconfirmedCooldown || lock.released != 0):
				t.Errorf("released = %d, cooled = %v; want held for %s", lock.released, lock.cooled, domain.KeywordTargetingUnconfirmedCooldown)
			case !tc.cooled && (lock.released != 1 || len(lock.cooled) != 0):
				t.Errorf("released = %d, cooled = %v; a settled outcome must release at once", lock.released, lock.cooled)
			}
		})
	}
}

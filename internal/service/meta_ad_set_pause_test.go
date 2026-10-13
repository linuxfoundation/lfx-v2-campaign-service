// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// LFXV2-2665 product decision (2026-10-13): a deliberate pause of the campaign's RECORDED ad set,
// made through toggle-meta-ad-set-status, is kept on the campaign row so a campaign resume leaves
// it paused. These tests pin the service's half: when the record is set, kept and cleared.

const unconfirmedAdSetText = "the ad set status change is unconfirmed — it may or may not have been applied on Meta; read the ad sets before retrying"

// recordedMetaRow is a dispatched Meta campaign: its Result records ad set 888.
func recordedMetaRow() *model.Campaign {
	c := metaCampaignRow()
	c.Result = json.RawMessage(`{"AccountID":"act_777","CampaignID":"555","AdSetID":"888"}`)
	return c
}

func heldRow(t *testing.T) *model.Campaign {
	t.Helper()
	c := recordedMetaRow()
	res, err := model.WithMetaUserPausedAdSet(c.Result, "888")
	if err != nil {
		t.Fatal(err)
	}
	c.Result = res
	return c
}

func held(repo *adSetRepo) bool {
	repo.dataMu.Lock()
	defer repo.dataMu.Unlock()
	return model.MetaAdSetHeldPaused(repo.got.Result, "888")
}

func TestToggleMetaAdSetStatus_PauseOfTheRecordedAdSetIsRecordedBeforeTheWrite(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if err != nil {
		t.Fatalf("ToggleMetaAdSetStatus: %v", err)
	}
	if d.hookCalls != 1 || repo.replaces != 1 || !held(repo) {
		t.Fatalf("hook calls %d, row writes %d, held %v; want the pause recorded once, through the hook", d.hookCalls, repo.replaces, held(repo))
	}
	// The record bumped the version, so the ETag is the new one.
	if res.Etag == nil || *res.Etag != `"4"` || len(repo.indexPayloads) != 1 {
		t.Fatalf("etag = %v, index payloads = %d", res.Etag, len(repo.indexPayloads))
	}
	// Every other key of the create path's blob survives.
	if model.MetaRecordedAdSetID(repo.got.Result) != "888" || !strings.Contains(string(repo.got.Result), `"AccountID":"act_777"`) {
		t.Fatalf("result = %s", repo.got.Result)
	}
}

func TestToggleMetaAdSetStatus_AlreadyPausedIsStillRecorded(t *testing.T) {
	// The campaign's own pause already paused the ad set; pausing it here is still deliberate.
	d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetAlreadyInState, PreviousStatus: "PAUSED"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if err != nil || res.Outcome != model.MetaAdSetAlreadyInState || *res.Etag != `"4"` || !held(repo) {
		t.Fatalf("got %+v, %v, held %v", res, err, held(repo))
	}
}

// UNCONFIRMED: the record written before the write stays — unknown is treated as paused — and the
// answer is the lever's fixed 503 with the lock held for the cooldown.
func TestToggleMetaAdSetStatus_UnconfirmedPauseStaysOnRecord(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: adSetUnconfirmedErr{msg: "meta API POST /888 failed (500)"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if res != nil || !isStatus(err, 503) || errMessage(err) != unconfirmedAdSetText {
		t.Fatalf("got %+v, %T %q", res, err, errMessage(err))
	}
	if !held(repo) || repo.replaces != 1 || repo.cooldowns != 1 {
		t.Fatalf("held %v, row writes %d, cooldowns %d; want the pause kept on record", held(repo), repo.replaces, repo.cooldowns)
	}
}

// A write that definitely changed nothing withdraws the record it wrote.
func TestToggleMetaAdSetStatus_DefinitelyRefusedPauseWithdrawsTheRecord(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: errors.New("meta refused the write")}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if !isStatus(err, 503) || !strings.Contains(errMessage(err), "nothing was changed") {
		t.Fatalf("err = %T %q", err, errMessage(err))
	}
	if held(repo) || repo.replaces != 2 || repo.cooldowns != 0 {
		t.Fatalf("held %v, row writes %d, cooldowns %d; want the record written then withdrawn", held(repo), repo.replaces, repo.cooldowns)
	}
}

// When the record cannot be written, nothing is sent.
func TestToggleMetaAdSetStatus_PauseNotRecordedIsNotSent(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"db failure":  {errors.New("connection reset"), 503},
		"row changed": {fmt.Errorf("replace: %w", domain.ErrPreconditionFailed), 409},
		"row deleted": {fmt.Errorf("replace: %w", domain.ErrNotFound), 409},
	} {
		t.Run(name, func(t *testing.T) {
			d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied}}
			s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
			repo.replaceErrs = []error{tc.err}
			res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
			if res != nil || !isStatus(err, tc.want) || !strings.Contains(errMessage(err), "nothing was changed on Meta") {
				t.Fatalf("got %+v, %T %q; want %d saying nothing was changed", res, err, errMessage(err), tc.want)
			}
			if held(repo) || repo.cooldowns != 0 {
				t.Fatalf("held %v, cooldowns %d", held(repo), repo.cooldowns)
			}
		})
	}
}

func TestToggleMetaAdSetStatus_ConfirmedResumeClearsTheRecord(t *testing.T) {
	for _, outcome := range []string{model.MetaAdSetApplied, model.MetaAdSetAlreadyInState} {
		t.Run(outcome, func(t *testing.T) {
			d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: outcome, PreviousStatus: "PAUSED"}}
			s, repo := adSetService(heldRow(t), model.ProviderMetaAds, d)
			res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
			if err != nil || held(repo) || *res.Etag != `"4"` || repo.replaces != 1 {
				t.Fatalf("got %+v, %v; held %v, writes %d", res, err, held(repo), repo.replaces)
			}
			if d.hookCalls != 0 {
				t.Fatalf("a resume recorded something before its write")
			}
		})
	}
}

// An UNCONFIRMED resume does not clear the record: the ad set may still be paused, and keeping it
// held is the direction that never spends unasked.
func TestToggleMetaAdSetStatus_UnconfirmedResumeKeepsTheRecord(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: adSetUnconfirmedErr{msg: "timeout"}}
	s, repo := adSetService(heldRow(t), model.ProviderMetaAds, d)
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
	if !isStatus(err, 503) || errMessage(err) != unconfirmedAdSetText || !held(repo) || repo.replaces != 0 {
		t.Fatalf("err %q, held %v, writes %d", errMessage(err), held(repo), repo.replaces)
	}
}

func TestToggleMetaAdSetStatus_ResumedButClearingFailedSaysSo(t *testing.T) {
	d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "PAUSED"}}
	s, repo := adSetService(heldRow(t), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{errors.New("db down")}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
	if msg := errMessage(err); !isStatus(err, 503) || !strings.Contains(msg, "WAS resumed on Meta") || !strings.Contains(msg, "leaves this ad set paused") {
		t.Fatalf("err = %T %q", err, msg)
	}
	if !held(repo) {
		t.Fatal("the record was lost")
	}
}

// Nothing is recorded for an ad set the campaign cascade never re-activates: a hand-added one,
// or any ad set of an ADOPTED campaign (which records none) — #288's behaviour, unchanged.
func TestToggleMetaAdSetStatus_UntrackedAdSetsRecordNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		row   *model.Campaign
		adSet string
	}{
		"hand-added ad set": {recordedMetaRow(), "889"},
		"adopted campaign": {func() *model.Campaign {
			c := metaCampaignRow()
			c.Result = json.RawMessage(`{"AccountID":"act_777","CampaignID":"555"}`)
			return c
		}(), "888"},
	} {
		t.Run(name, func(t *testing.T) {
			d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: tc.adSet, Outcome: model.MetaAdSetApplied}}
			s, repo := adSetService(tc.row, model.ProviderMetaAds, d)
			res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", tc.adSet, "PAUSED"))
			if err != nil || *res.Etag != `"3"` || repo.replaces != 0 || d.hookCalls != 0 {
				t.Fatalf("got %+v, %v; writes %d, hook %d", res, err, repo.replaces, d.hookCalls)
			}
		})
	}
}

// pauseAndCampaignDispatcher implements the ad-set lever AND the campaign toggle, so the two can
// be raced through the campaign's write lock. The lever parks after its hook until released.
type pauseAndCampaignDispatcher struct {
	adSetDispatcher
	inWrite        chan struct{}
	release        chan struct{}
	campaignCalls  int
	campaignHeld   bool
	campaignStatus string
}

func (d *pauseAndCampaignDispatcher) ToggleMetaAdSetStatus(ctx context.Context, projectID string, platform model.Provider, c *model.Campaign, adSetID, status string, beforeWrite MetaAdSetBeforeWrite) (*model.MetaAdSetStatusResult, error) {
	if beforeWrite != nil {
		if err := beforeWrite(ctx); err != nil {
			return nil, err
		}
	}
	close(d.inWrite)
	<-d.release
	return &model.MetaAdSetStatusResult{AdSetID: adSetID, Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}, nil
}

func (d *pauseAndCampaignDispatcher) ToggleStatus(_ context.Context, _ string, _ model.Provider, c *model.Campaign, status string) error {
	d.campaignCalls++
	d.campaignHeld = model.MetaAdSetHeldPaused(c.Result, "888")
	d.campaignStatus = status
	return nil
}

// The lever and the campaign toggle share the campaign's write lock: a campaign resume racing a
// pause in flight is refused (409, never reaching Meta), a resume on the ETag read before the
// pause is 412, and the next resume sees the record — so it cannot cascade over the pause.
func TestToggleMetaAdSetStatus_RacesTheCampaignToggleThroughTheWriteLock(t *testing.T) {
	row := recordedMetaRow()
	row.Status = model.CampaignRunPaused
	d := &pauseAndCampaignDispatcher{inWrite: make(chan struct{}), release: make(chan struct{})}
	s, repo := adSetService(row, model.ProviderMetaAds, d)
	// Every wait below is bounded, and release is closed on cleanup whatever happens, so a
	// regression fails this test instead of hanging the package or leaking the goroutine.
	var releaseOnce sync.Once
	releaseLever := func() { releaseOnce.Do(func() { close(d.release) }) }
	t.Cleanup(releaseLever)
	const wait = 5 * time.Second

	type result struct {
		res *briefs.MetaAdSetStatusChange
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
		done <- result{res, err}
	}()
	select {
	case <-d.inWrite:
	case r := <-done:
		t.Fatalf("the pause returned before reaching its write: %+v, %v", r.res, r.err)
	case <-time.After(wait):
		t.Fatal("the pause never reached its write")
	}

	resume := func(etag string) error {
		_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
			ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &etag, Status: model.CampaignRunActive,
		})
		return err
	}
	// The pause is mid-write holding the lock, its record already at version 4.
	if err := resume("4"); !isStatus(err, 409) || d.campaignCalls != 0 {
		t.Fatalf("resume during the pause: %T %v, campaign toggles %d; want a 409 that never reaches Meta", err, err, d.campaignCalls)
	}
	releaseLever()
	var r result
	select {
	case r = <-done:
	case <-time.After(wait):
		t.Fatal("the pause did not finish after its write was released")
	}
	if r.err != nil || *r.res.Etag != `"4"` {
		t.Fatalf("pause: %+v, %v", r.res, r.err)
	}
	if err := resume("3"); !isStatus(err, 412) || d.campaignCalls != 0 {
		t.Fatalf("resume on the pre-pause ETag: %T %v; want 412", err, err)
	}
	if err := resume("4"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if d.campaignCalls != 1 || !d.campaignHeld || d.campaignStatus != model.CampaignRunActive || !held(repo) {
		t.Fatalf("campaign toggles %d, saw the record %v, status %q; the resume must see the pause on record", d.campaignCalls, d.campaignHeld, d.campaignStatus)
	}
}

// ---- ambiguous commits of the record (PR #309) -----------------------------
//
// A ReplaceCampaign whose COMMIT errored (domain.ErrWriteOutcomeUnknown) may have landed. The one
// state the lever must never produce is a pause on record over an ad set that was never paused.

// The dangerous case: the record landed though its commit errored. Before the fix the pause was
// then suppressed, leaving the record over a live ad set; now it is reconciled as landed and sent.
func TestToggleMetaAdSetStatus_AmbiguousRecordThatLandedStillSendsThePause(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{errLandedButAmbiguous}
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if err != nil || *res.Etag != `"4"` {
		t.Fatalf("got %+v, %v", res, err)
	}
	if d.sent != 1 || !held(repo) {
		t.Fatalf("sent %d, held %v; a record that landed must be followed by the pause", d.sent, held(repo))
	}
}

// Rolled back: the reconciling write records it, and the pause is sent.
func TestToggleMetaAdSetStatus_AmbiguousRecordThatRolledBackIsWrittenAndSent(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{ambiguousCommit()}
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if err != nil || *res.Etag != `"4"` || d.sent != 1 || !held(repo) || repo.replaces != 2 {
		t.Fatalf("got %+v, %v; sent %d, held %v, writes %d", res, err, d.sent, held(repo), repo.replaces)
	}
}

// Unreconcilable (the re-read fails too): the pause is still SENT — never suppressed over a
// record that may exist — and the answer says the record could not be confirmed.
func TestToggleMetaAdSetStatus_UnreconcilableRecordStillSendsThePause(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{errLandedButAmbiguous}
	repo.verifyErr = errors.New("db down")
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); res != nil || !isStatus(err, 503) || !strings.Contains(msg, "WAS paused on Meta") || !strings.Contains(msg, "could not be confirmed") {
		t.Fatalf("got %+v, %T %q", res, err, msg)
	}
	if d.sent != 1 || !held(repo) {
		t.Fatalf("sent %d, held %v", d.sent, held(repo))
	}
}

// A definite pause failure after an ambiguous-but-landed record withdraws it.
func TestToggleMetaAdSetStatus_RefusedPauseWithdrawsALandedAmbiguousRecord(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: errors.New("meta refused the write")}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{errLandedButAmbiguous}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if !isStatus(err, 503) || !strings.Contains(errMessage(err), "nothing was changed") || held(repo) {
		t.Fatalf("err %q, held %v", errMessage(err), held(repo))
	}
}

// If the withdrawal cannot be established, the answer says the record may claim a pause over an
// active ad set — never "nothing was changed".
func TestToggleMetaAdSetStatus_UnwithdrawableRecordIsReportedLoudly(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: errors.New("meta refused the write")}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{errLandedButAmbiguous}
	repo.verifyErr = errors.New("db down")
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); !isStatus(err, 503) || !strings.Contains(msg, "could not be withdrawn") || strings.Contains(msg, "nothing was changed") {
		t.Fatalf("err = %q", msg)
	}
}

// The withdrawal's own ambiguous commit is reconciled too.
func TestToggleMetaAdSetStatus_AmbiguousWithdrawalIsReconciled(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: errors.New("meta refused the write")}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{nil, errLandedButAmbiguous}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if !isStatus(err, 503) || !strings.Contains(errMessage(err), "nothing was changed") || held(repo) {
		t.Fatalf("err %q, held %v", errMessage(err), held(repo))
	}
}

// Clear-on-resume shares the path: a landed ambiguous clear is success; an unreconcilable one is
// a 503 saying it could not be confirmed.
func TestToggleMetaAdSetStatus_AmbiguousClearOnResume(t *testing.T) {
	t.Run("landed", func(t *testing.T) {
		d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "PAUSED"}}
		s, repo := adSetService(heldRow(t), model.ProviderMetaAds, d)
		repo.replaceErrs = []error{errLandedButAmbiguous}
		res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
		if err != nil || *res.Etag != `"4"` || held(repo) {
			t.Fatalf("got %+v, %v, held %v", res, err, held(repo))
		}
	})
	t.Run("unreconcilable", func(t *testing.T) {
		d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "PAUSED"}}
		s, repo := adSetService(heldRow(t), model.ProviderMetaAds, d)
		repo.replaceErrs = []error{errLandedButAmbiguous}
		repo.verifyErr = errors.New("db down")
		_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
		if msg := errMessage(err); !isStatus(err, 503) || !strings.Contains(msg, "WAS resumed on Meta") || !strings.Contains(msg, "could not be confirmed") {
			t.Fatalf("err = %q", msg)
		}
	})
}

// The claimed connection is unusable after the ambiguous commit: the claim is handed back and
// taken again (never a second pooled connection beside it), and the record is written on it.
func TestToggleMetaAdSetStatus_UnusableClaimedConnectionReTakesTheClaim(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{ambiguousCommit(), errors.New("conn closed")}
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if err != nil || *res.Etag != `"4"` || d.sent != 1 || !held(repo) || repo.replaces != 3 {
		t.Fatalf("got %+v, %v; sent %d, held %v, writes %d", res, err, d.sent, held(repo), repo.replaces)
	}
	// The record was written by the reconciliation itself, on a claim taken AGAIN — not by a
	// later settlement under the original (unusable) claim.
	if repo.claims != 2 || repo.replaceTokens[2].CampaignID == "" || repo.replaceTokens[2].Handle() == repo.replaceTokens[0].Handle() {
		t.Fatalf("claims %d; third write under %+v, first under %+v — want it under a new claim", repo.claims, repo.replaceTokens[2], repo.replaceTokens[0])
	}
	// The re-taken claim was released: a new claim succeeds.
	if _, tok, cerr := repo.ClaimCampaignVersion(context.Background(), "cncf", "b1", "c1", 4); cerr != nil {
		t.Fatalf("claim after the call: %v", cerr)
	} else {
		_ = repo.ReleaseCampaignLock(context.Background(), tok)
	}
}

// ---- an UNCONFIRMED Meta answer after an unconfirmed record (PR #309) ------

// adSetAndCampaignDispatcher adds the campaign toggle to the ad-set fake, recording whether the
// row it was handed holds the ad set paused.
type adSetAndCampaignDispatcher struct {
	adSetDispatcher
	campaignHeld []bool
}

func (d *adSetAndCampaignDispatcher) ToggleStatus(_ context.Context, _ string, _ model.Provider, c *model.Campaign, _ string) error {
	d.campaignHeld = append(d.campaignHeld, model.MetaAdSetHeldPaused(c.Result, "888"))
	return nil
}

// The record could not be confirmed before the pause was sent (every attempt's commit was
// ambiguous, though the claim was re-taken), and Meta answers UNCONFIRMED. Unknown counts as paused, so the
// record is SETTLED before the 503 — and a later campaign resume keeps the ad set paused.
func TestToggleMetaAdSetStatus_UnconfirmedPauseAfterAnUnconfirmedRecordSettlesIt(t *testing.T) {
	for name, d := range map[string]*adSetAndCampaignDispatcher{
		"meta answers UNCONFIRMED":      {adSetDispatcher: adSetDispatcher{sends: true, toggleErr: adSetUnconfirmedErr{msg: "timeout"}}},
		"meta answers no known outcome": {adSetDispatcher: adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{Outcome: "DONE"}}},
	} {
		t.Run(name, func(t *testing.T) {
			s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
			// The first commit and its retry on the claimed connection are ambiguous; the claim is
			// re-taken, and the write on it is ambiguous too: the record is "unknown", the claim
			// is held, and the pause is sent.
			repo.replaceErrs = []error{ambiguousCommit(), ambiguousCommit(), ambiguousCommit()}
			res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
			if res != nil || !isStatus(err, 503) || errMessage(err) != unconfirmedAdSetText || d.sent != 1 {
				t.Fatalf("got %+v, %T %q, sent %d", res, err, errMessage(err), d.sent)
			}
			if !held(repo) || repo.cooldowns != 1 {
				t.Fatalf("held %v, cooldowns %d; the unconfirmed pause must be on record", held(repo), repo.cooldowns)
			}
			etag := fmt.Sprint(repo.got.Version)
			if _, terr := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
				ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &etag, Status: model.CampaignRunActive,
			}); terr != nil || len(d.campaignHeld) != 1 || !d.campaignHeld[0] {
				t.Fatalf("campaign resume: %T %q, saw held %v; it must keep the ad set paused", terr, errMessage(terr), d.campaignHeld)
			}
		})
	}
}

// If even the settlement cannot be confirmed, the 503 says the record is unconfirmed too.
func TestToggleMetaAdSetStatus_UnconfirmedPauseWithAnUnsettleableRecordSaysSo(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggleErr: adSetUnconfirmedErr{msg: "timeout"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{ambiguousCommit(), ambiguousCommit(), ambiguousCommit()}
	repo.verifyErr = errors.New("db down")
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); !isStatus(err, 503) || !strings.Contains(msg, "unconfirmed") || !strings.Contains(msg, "whether its pause was recorded on the campaign could not be confirmed") || repo.cooldowns != 1 {
		t.Fatalf("err %q, cooldowns %d", msg, repo.cooldowns)
	}
}

// A CONFIRMED rollback of the record (the server rejected its commit) is a definite failure:
// nothing is recorded, so nothing is sent.
func TestToggleMetaAdSetStatus_ConfirmedRollbackOfTheRecordSendsNothing(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{errors.New("replace campaign: commit rolled back by the server: ERROR: deferred failure (SQLSTATE P0001)")}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if !isStatus(err, 503) || !strings.Contains(errMessage(err), "nothing was changed on Meta") || d.sent != 0 || held(repo) || repo.replaces != 1 {
		t.Fatalf("err %q, sent %d, held %v, writes %d", errMessage(err), d.sent, held(repo), repo.replaces)
	}
}

// ---- the claim given up to reconcile (PR #309) -------------------------------

const claimLostText = "the campaign's write lock was lost while recording the ad set's pause, so nothing further was sent to Meta"

// The claim could not be re-taken after it was given up: fail CLOSED — the pause is not sent and
// nothing more is written.
func TestToggleMetaAdSetStatus_ClaimNotReTakenFailsClosed(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{ambiguousCommit(), errors.New("conn closed")}
	repo.reclaimErr = domain.ErrCampaignWriteInProgress
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); !isStatus(err, 503) || !strings.HasPrefix(msg, claimLostText) || d.sent != 0 || repo.replaces != 2 || repo.cooldowns != 0 {
		t.Fatalf("err %q, sent %d, writes %d, cooldowns %d; want a fail-closed 503 and nothing sent", msg, d.sent, repo.replaces, repo.cooldowns)
	}
}

// Re-claimed at the original version, the first attempt is PROVEN not to have landed, so a
// non-ambiguous failure of the write on the new claim is definite: nothing is sent.
func TestToggleMetaAdSetStatus_DefiniteFailureOnTheReTakenClaimSendsNothing(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{ambiguousCommit(), errors.New("conn closed"), errors.New("replace campaign: commit rolled back by the server")}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); !isStatus(err, 503) || !strings.Contains(msg, "nothing was changed on Meta") || d.sent != 0 || held(repo) {
		t.Fatalf("err %q, sent %d, held %v", msg, d.sent, held(repo))
	}
}

// Re-claimed only at the NEXT version, and that row carries no record: another writer moved the
// row and the first attempt did not land — definite, nothing sent.
func TestToggleMetaAdSetStatus_ReTakenAtTheNextVersionWithoutTheRecordSendsNothing(t *testing.T) {
	d := &adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{ambiguousCommit(), errors.New("conn closed")}
	repo.onRelease = func() {
		// Another writer's change lands in the gap, without the record.
		repo.dataMu.Lock()
		repo.got.Version++
		repo.dataMu.Unlock()
	}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); !isStatus(err, 409) || !strings.Contains(msg, "nothing was changed on Meta") || d.sent != 0 || held(repo) {
		t.Fatalf("err %T %q, sent %d, held %v", err, msg, d.sent, held(repo))
	}
}

// gapDispatcher: the ad-set fake plus a campaign toggle that parks inside the platform call (so
// it HOLDS the claim) until released, recording whether its row held the ad set paused.
type gapDispatcher struct {
	adSetDispatcher
	inToggle     chan struct{}
	release      chan struct{}
	campaignHeld []bool
}

func (d *gapDispatcher) ToggleStatus(_ context.Context, _ string, _ model.Provider, c *model.Campaign, _ string) error {
	d.campaignHeld = append(d.campaignHeld, model.MetaAdSetHeldPaused(c.Result, "888"))
	close(d.inToggle)
	<-d.release
	return nil
}

// A campaign resume takes the claim in the gap the reconciliation opened. The lever must not
// carry on: it fails closed and the pause is never sent, while the resume proceeds on a row
// without the record — so no pause can be lost under it.
func TestToggleMetaAdSetStatus_ConcurrentToggleInTheReclaimGapStopsThePause(t *testing.T) {
	d := &gapDispatcher{
		adSetDispatcher: adSetDispatcher{sends: true, toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied}},
		inToggle:        make(chan struct{}),
		release:         make(chan struct{}),
	}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	var releaseOnce sync.Once
	releaseToggle := func() { releaseOnce.Do(func() { close(d.release) }) }
	t.Cleanup(releaseToggle)
	const wait = 5 * time.Second

	resumed := make(chan error, 1)
	repo.replaceErrs = []error{ambiguousCommit(), errors.New("conn closed")}
	repo.onRelease = func() {
		go func() {
			etag := "3"
			_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
				ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &etag, Status: model.CampaignRunActive,
			})
			resumed <- err
		}()
		select {
		case <-d.inToggle:
		case err := <-resumed:
			t.Errorf("the campaign resume finished before taking the claim: %v", err)
		case <-time.After(wait):
			t.Error("the campaign resume never took the claim")
		}
	}
	_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if msg := errMessage(err); !isStatus(err, 503) || !strings.HasPrefix(msg, claimLostText) || d.sent != 0 {
		t.Fatalf("err %q, sent %d; the pause must not be sent once another writer holds the claim", msg, d.sent)
	}
	releaseToggle()
	select {
	case rerr := <-resumed:
		if rerr != nil {
			t.Fatalf("campaign resume: %v", rerr)
		}
	case <-time.After(wait):
		t.Fatal("the campaign resume did not finish")
	}
	if len(d.campaignHeld) != 1 || d.campaignHeld[0] || held(repo) {
		t.Fatalf("resume saw held %v, row held %v; the resume ran on a row without the record and no pause was sent", d.campaignHeld, held(repo))
	}
}

// Without a claim — the zero token a lost re-claim leaves — no record write and no settlement read
// may run: either would go through a lock-free pooled connection and bypass the writer that now
// holds the claim. Both refuse with errMetaAdSetClaimLost and touch nothing.
func TestMetaAdSetRecord_RefusesToRunWithoutTheClaim(t *testing.T) {
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, &adSetDispatcher{})
	row := recordedMetaRow()
	zero := domain.CampaignLockToken{}
	if _, err := s.settleMetaAdSetRecord(context.Background(), repo, row, "888", 3, &zero); !errors.Is(err, errMetaAdSetClaimLost) {
		t.Fatalf("settle without a claim: %v; want errMetaAdSetClaimLost", err)
	}
	if _, err := s.writeMetaAdSetRecord(context.Background(), repo, row, "888", 3, &zero); !errors.Is(err, errMetaAdSetClaimLost) {
		t.Fatalf("write without a claim: %v; want errMetaAdSetClaimLost", err)
	}
	if err := s.withdrawMetaAdSetRecord(context.Background(), repo, row, 3, &zero, true, "888"); !errors.Is(err, errMetaAdSetClaimLost) {
		t.Fatalf("withdraw without a claim: %v; want errMetaAdSetClaimLost", err)
	}
	if repo.replaces != 0 || repo.verifies != 0 || held(repo) {
		t.Fatalf("writes %d, reads %d, held %v; nothing may run without the claim", repo.replaces, repo.verifies, held(repo))
	}
}

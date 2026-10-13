// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

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

	type result struct {
		res *briefs.MetaAdSetStatusChange
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
		done <- result{res, err}
	}()
	<-d.inWrite

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
	close(d.release)
	r := <-done
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

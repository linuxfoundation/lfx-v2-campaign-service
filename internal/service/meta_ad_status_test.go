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
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// #550 gap 14: the per-ad lever's service half — the flag, and when the per-ad record is set, kept,
// withdrawn and cleared (the ad-set lever's rules).

type adToggleDispatcher struct {
	toggleErr error
	toggle    *model.MetaAdStatusResult
	reached   int
	gotAdSet  string
	gotAd     string
	sends     bool
	hookCalls int
}

func (d *adToggleDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *adToggleDispatcher) ToggleMetaAdStatus(ctx context.Context, _ string, _ model.Provider, _ *model.Campaign, adSetID, adID, _ string, beforeWrite MetaAdSetBeforeWrite) (*model.MetaAdStatusResult, error) {
	d.reached++
	d.gotAdSet, d.gotAd = adSetID, adID
	if d.sends && beforeWrite != nil {
		d.hookCalls++
		if err := beforeWrite(ctx); err != nil {
			return nil, fmt.Errorf("nothing was sent: %w", err)
		}
	}
	return d.toggle, d.toggleErr
}

func adPayload(ifMatch, adSet, ad, status string) *briefs.ToggleMetaAdStatusPayload {
	p := &briefs.ToggleMetaAdStatusPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", AdSetID: adSet, AdID: ad, Status: status}
	if ifMatch != "" {
		p.IfMatch = &ifMatch
	}
	return p
}

func adHeld(repo *adSetRepo, adID string) bool {
	repo.dataMu.Lock()
	defer repo.dataMu.Unlock()
	return model.MetaAdHeldPaused(repo.got.Result, adID)
}

func adHeldRow(t *testing.T) *model.Campaign {
	t.Helper()
	c := recordedMetaRow()
	res, err := model.WithMetaUserPausedAd(c.Result, "901", true)
	if err != nil {
		t.Fatal(err)
	}
	c.Result = res
	return c
}

func TestToggleMetaAdStatus_FlagOffIs400BeforeAnything(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Setenv(constants.EnvMetaAdPauseEnabled, v)
		d := &adToggleDispatcher{}
		s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
		_, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED"))
		if !isStatus(err, 400) || !strings.Contains(errMessage(err), "META_AD_PAUSE_ENABLED") || d.reached != 0 || repo.replaces != 0 {
			t.Fatalf("flag %q: %T %q, reached %d", v, err, errMessage(err), d.reached)
		}
	}
}

func TestToggleMetaAdStatus_PauseOfAnAdOfTheRecordedAdSetIsRecordedBeforeTheWrite(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	d := &adToggleDispatcher{sends: true, toggle: &model.MetaAdStatusResult{AdID: "901", PreviousStatus: "ACTIVE", Outcome: model.MetaAdSetApplied}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED"))
	if err != nil || res.Outcome != model.MetaAdSetApplied || res.AdID != "901" || res.AdSetID != "888" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if d.hookCalls != 1 || repo.replaces != 1 || !adHeld(repo, "901") || *res.Etag != `"4"` {
		t.Fatalf("hook %d, writes %d, held %v, etag %v", d.hookCalls, repo.replaces, adHeld(repo, "901"), res.Etag)
	}
	if model.MetaRecordedAdSetID(repo.got.Result) != "888" {
		t.Fatalf("result = %s", repo.got.Result)
	}
}

func TestToggleMetaAdStatus_AlreadyPausedIsStillRecorded(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	d := &adToggleDispatcher{toggle: &model.MetaAdStatusResult{AdID: "901", PreviousStatus: "PAUSED", Outcome: model.MetaAdSetAlreadyInState}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED"))
	if err != nil || !adHeld(repo, "901") || *res.Etag != `"4"` {
		t.Fatalf("got %+v, %v, held %v", res, err, adHeld(repo, "901"))
	}
}

func TestToggleMetaAdStatus_UnconfirmedPauseStaysOnRecord(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	d := &adToggleDispatcher{sends: true, toggleErr: adSetUnconfirmedErr{msg: "meta API POST /901 failed (500)"}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	_, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED"))
	if !isStatus(err, 503) || errMessage(err) != "the ad status change is unconfirmed — it may or may not have been applied on Meta; read the ads before retrying" {
		t.Fatalf("err = %T %q", err, errMessage(err))
	}
	if !adHeld(repo, "901") || repo.cooldowns != 1 {
		t.Fatalf("held %v, cooldowns %d", adHeld(repo, "901"), repo.cooldowns)
	}
}

func TestToggleMetaAdStatus_DefinitelyRefusedPauseWithdrawsTheRecord(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	d := &adToggleDispatcher{sends: true, toggleErr: errors.New("meta refused the write")}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	_, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED"))
	if !isStatus(err, 503) || !strings.Contains(errMessage(err), "the ad's status could not be changed; nothing was changed on Meta") {
		t.Fatalf("err = %T %q", err, errMessage(err))
	}
	if adHeld(repo, "901") || repo.replaces != 2 {
		t.Fatalf("held %v, writes %d; want written then withdrawn", adHeld(repo, "901"), repo.replaces)
	}
}

func TestToggleMetaAdStatus_PauseNotRecordedIsNotSent(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	d := &adToggleDispatcher{sends: true, toggle: &model.MetaAdStatusResult{AdID: "901", Outcome: model.MetaAdSetApplied}}
	s, repo := adSetService(recordedMetaRow(), model.ProviderMetaAds, d)
	repo.replaceErrs = []error{fmt.Errorf("replace: %w", domain.ErrPreconditionFailed)}
	_, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED"))
	if !isStatus(err, 409) || !strings.Contains(errMessage(err), "the ad's pause was not sent") || adHeld(repo, "901") {
		t.Fatalf("err = %T %q, held %v", err, errMessage(err), adHeld(repo, "901"))
	}
}

func TestToggleMetaAdStatus_ConfirmedResumeClearsOnlyThatAd(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	row := adHeldRow(t)
	res2, _ := model.WithMetaUserPausedAd(row.Result, "902", true)
	row.Result = res2
	d := &adToggleDispatcher{sends: true, toggle: &model.MetaAdStatusResult{AdID: "901", PreviousStatus: "PAUSED", Outcome: model.MetaAdSetApplied}}
	s, repo := adSetService(row, model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "ACTIVE"))
	if err != nil || adHeld(repo, "901") || !adHeld(repo, "902") || *res.Etag != `"4"` {
		t.Fatalf("got %+v, %v; held 901 %v, 902 %v", res, err, adHeld(repo, "901"), adHeld(repo, "902"))
	}
}

func TestToggleMetaAdStatus_UnconfirmedResumeKeepsTheRecord(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	d := &adToggleDispatcher{sends: true, toggleErr: adSetUnconfirmedErr{msg: "timeout"}}
	s, repo := adSetService(adHeldRow(t), model.ProviderMetaAds, d)
	_, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "ACTIVE"))
	if !isStatus(err, 503) || !adHeld(repo, "901") || repo.replaces != 0 {
		t.Fatalf("err = %v, held %v, writes %d", err, adHeld(repo, "901"), repo.replaces)
	}
}

// An ad of another ad set, and anything on an adopted row, records nothing.
func TestToggleMetaAdStatus_UntrackedAdsRecordNothing(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	for name, tc := range map[string]struct {
		row   *model.Campaign
		adSet string
	}{
		"another ad set": {recordedMetaRow(), "889"},
		"adopted row":    {metaCampaignRow(), "888"},
	} {
		t.Run(name, func(t *testing.T) {
			d := &adToggleDispatcher{sends: true, toggle: &model.MetaAdStatusResult{AdID: "901", Outcome: model.MetaAdSetApplied}}
			s, repo := adSetService(tc.row, model.ProviderMetaAds, d)
			res, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", tc.adSet, "901", "PAUSED"))
			if err != nil || d.hookCalls != 0 || repo.replaces != 0 || adHeld(repo, "901") || *res.Etag != `"3"` {
				t.Fatalf("got %+v, %v; hook %d, writes %d", res, err, d.hookCalls, repo.replaces)
			}
		})
	}
}

func TestToggleMetaAdStatus_ValidationAndClassification(t *testing.T) {
	t.Setenv(constants.EnvMetaAdPauseEnabled, "true")
	for _, tc := range []struct {
		name    string
		payload *briefs.ToggleMetaAdStatusPayload
		err     error
		code    int
		want    string
	}{
		{"bad ad id", adPayload("3", "888", "0901", "PAUSED"), nil, 400, "ad_id must be a Meta ad id"},
		{"bad ad set id", adPayload("3", "x", "901", "PAUSED"), nil, 400, "ad_set_id must be a Meta ad set id"},
		{"bad status", adPayload("3", "888", "901", "DELETED"), nil, 400, "status must be"},
		{"stale etag", adPayload("2", "888", "901", "PAUSED"), nil, 412, "ETag"},
		{"not in the ad set", adPayload("3", "888", "901", "PAUSED"), domain.ErrMetaAdNotInAdSet, 409, "the ad does not belong to this ad set"},
		{"unrecorded activate", adPayload("3", "889", "901", "ACTIVE"), domain.ErrMetaAdNotRecorded, 409, "only an ad of the ad set this service created"},
		{"unwritable", adPayload("3", "888", "901", "PAUSED"), domain.ErrMetaAdUnwritable, 409, "the ad is deleted or archived"},
		{"adopted activate", adPayload("3", "888", "901", "ACTIVE"), ErrCampaignNotProvisioned, 409, "this campaign's ads cannot be activated here"},
		{"no connection", adPayload("3", "888", "901", "PAUSED"), domain.ErrConnectionAbsent, 404, "no connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &adToggleDispatcher{toggleErr: tc.err, toggle: &model.MetaAdStatusResult{Outcome: model.MetaAdSetApplied}}
			if tc.err != nil {
				d.toggle = nil
			}
			s, _ := adSetService(adHeldRow(t), model.ProviderMetaAds, d)
			_, err := s.ToggleMetaAdStatus(context.Background(), tc.payload)
			if !isStatus(err, tc.code) || !strings.Contains(errMessage(err), tc.want) {
				t.Fatalf("err = %T %q; want %d containing %q", err, errMessage(err), tc.code, tc.want)
			}
		})
	}
	// A platform without the capability is 400 before the claim.
	s, _ := adSetService(recordedMetaRow(), model.ProviderMetaAds, &noAdSetDispatcher{})
	if _, err := s.ToggleMetaAdStatus(context.Background(), adPayload("3", "888", "901", "PAUSED")); !isStatus(err, 400) || !strings.Contains(errMessage(err), "Meta campaigns only") {
		t.Fatalf("err = %T %q", err, errMessage(err))
	}
}

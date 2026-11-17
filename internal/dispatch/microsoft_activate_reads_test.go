// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// msFiveGroupCampaign records five created ad groups: the first (654) plus four further ones
// (655-658), so ACTIVATE makes five live-keyword reads.
func msFiveGroupCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321",
		Result: json.RawMessage(`{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987","keywordIds":["701"],"adGroups":[
			{"nameSuffix":"A","adGroupId":"654","adId":"987","keywordIds":["701"],"status":"created"},
			{"nameSuffix":"B","adGroupId":"655","adId":"988","keywordIds":["702"],"status":"created"},
			{"nameSuffix":"C","adGroupId":"656","adId":"989","keywordIds":["703"],"status":"created"},
			{"nameSuffix":"D","adGroupId":"657","adId":"990","keywordIds":["704"],"status":"created"},
			{"nameSuffix":"E","adGroupId":"658","adId":"991","keywordIds":["705"],"status":"created"}]}`),
	}
}

const msFiveGroupsLive = `{"Keywords":[{"Id":701,"Status":"Paused"},{"Id":702,"Status":"Paused"},{"Id":703,"Status":"Paused"},{"Id":704,"Status":"Paused"},{"Id":705,"Status":"Paused"}]}`

// msSlowReads answers every keyword read after delay, and every write at once.
func msSlowReads(delay time.Duration) func(method, path string) (int, string) {
	return func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			time.Sleep(delay)
			return http.StatusOK, msFiveGroupsLive
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	}
}

func assertDefiniteAndUnchanged(t *testing.T, err error, calls []msKwCall) {
	t.Helper()
	if err == nil {
		t.Fatal("ToggleStatus succeeded, want a refusal")
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		t.Errorf("no write was sent, so the refusal must be definite, got %v", err)
	}
	if m := msMutations(calls); len(m) != 0 {
		t.Errorf("a group was set Active before every keyword read finished: %+v", m)
	}
}

// Every ad group's live-keyword read runs BEFORE any status write.
func TestMicrosoftActivate_EveryKeywordReadPrecedesEveryWrite(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, msSlowReads(0))
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msFiveGroupCampaign(), model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	reads, firstWrite := 0, -1
	for i, c := range calls() {
		if c.Path == "Keywords/QueryByAdGroupId" {
			reads++
			if firstWrite >= 0 {
				t.Fatalf("keyword read %d came after a status write: %+v", reads, calls())
			}
		} else if c.Method == http.MethodPut && firstWrite < 0 {
			firstWrite = i
		}
	}
	if reads != 5 || firstWrite < 0 {
		t.Fatalf("reads = %d, first write at %d; want 5 reads then the writes", reads, firstWrite)
	}
}

// The read budget is SHARED by all the reads: five reads that each fit the budget alone but not
// together refuse the activate with nothing set Active.
func TestMicrosoftActivate_ReadsShareOneBudget(t *testing.T) {
	prev := microsoftActivateKeywordReadBudget
	microsoftActivateKeywordReadBudget = 200 * time.Millisecond
	t.Cleanup(func() { microsoftActivateKeywordReadBudget = prev })
	d, calls, _ := msLeverDispatcher(t, msSlowReads(70*time.Millisecond))
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msFiveGroupCampaign(), model.CampaignRunActive)
	assertDefiniteAndUnchanged(t, err, calls())
}

// The read budget is also capped at half of the caller's remaining deadline (the toggle's), so
// the reads cannot run the caller out of time for the writes: here the reads would together take
// most of the deadline, so the activate is refused, definitely, before the deadline expires.
func TestMicrosoftActivate_ReadsLeaveHalfTheDeadlineForTheWrites(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, msSlowReads(80*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := d.ToggleStatus(ctx, "proj", model.ProviderMicrosoftAds, msFiveGroupCampaign(), model.CampaignRunActive)
	if ctx.Err() != nil {
		t.Errorf("the refusal came after the caller's deadline: %v", ctx.Err())
	}
	assertDefiniteAndUnchanged(t, err, calls())
}

// A further group's failed read, or its "every keyword removed" answer, now lands before any
// write, so it is definite (and the latter keeps its 409 sentinel), never a partial toggle.
func TestMicrosoftActivate_LaterGroupReadFailureIsDefinite(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		notProvision bool
	}{
		{"read rejected", http.StatusBadRequest, `{"Errors":[{"Code":1201,"ErrorCode":"InvalidRequest","Message":"rejected"}]}`, false},
		{"every keyword removed", http.StatusOK, `{"Keywords":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				reads int
			)
			d, calls, _ := msLeverDispatcher(t, func(_, path string) (int, string) {
				if path == "Keywords/QueryByAdGroupId" {
					mu.Lock()
					reads++
					n := reads
					mu.Unlock()
					if n == 3 {
						return tc.status, tc.body
					}
					return http.StatusOK, msFiveGroupsLive
				}
				return http.StatusOK, `{"PartialErrors":[]}`
			})
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msFiveGroupCampaign(), model.CampaignRunActive)
			assertDefiniteAndUnchanged(t, err, calls())
			if tc.notProvision != errors.Is(err, domain.ErrCampaignNotProvisioned) {
				t.Errorf("ErrCampaignNotProvisioned = %v, want %v: %v", !tc.notProvision, tc.notProvision, err)
			}
			if !strings.Contains(err.Error(), "656") {
				t.Errorf("err does not name ad group 656: %v", err)
			}
		})
	}
}

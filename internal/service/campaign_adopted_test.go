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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
)

// adoptedRow is a campaign row as AdoptCampaign persists it: the adapter's provenance blob with
// the adoption marker stamped on.
func adoptedRow(platform model.Provider, status string) *model.Campaign {
	return &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: platform,
		PlatformCampaignID: "t3_c", Status: status, Version: 3,
		Result: json.RawMessage(`{"accountId":"a2_x","adopted":true}`),
	}
}

// indexedCampaignDoc decodes the CampaignDoc out of one co-committed index payload.
func indexedCampaignDoc(t *testing.T, payload []byte) indexer.CampaignDoc {
	t.Helper()
	var msg struct {
		Data indexer.CampaignDoc `json:"data"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("decode index payload %s: %v", payload, err)
	}
	return msg.Data
}

// Every platform's adoption stamps the one marker the flag is read from — including Google
// Ads, whose adapter records only the customer id — and keeps the provenance it was handed.
func TestAdoptCampaign_StampsTheAdoptedMarkerOnEveryPlatform(t *testing.T) {
	for _, platform := range []model.Provider{model.ProviderGoogleAds, model.ProviderRedditAds, model.ProviderLinkedInAds} {
		t.Run(string(platform), func(t *testing.T) {
			disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{
				ID: "1234567890", Name: "Adopted", Variant: model.VariantDefault,
				Result: json.RawMessage(`{"customerId":"1112223333"}`),
			}}
			s, camps := newAdoptService(t, platform, disp)
			s.SetIndexer(&failingIndexer{})
			p := adoptPayload()
			p.Platform = string(platform)

			res, err := s.AdoptCampaign(context.Background(), p)
			if err != nil {
				t.Fatalf("AdoptCampaign: %v", err)
			}
			if res.Adopted == nil || !*res.Adopted {
				t.Errorf("adopt response adopted = %v, want true", res.Adopted)
			}
			if len(camps.adopted) != 1 {
				t.Fatalf("persisted %d rows, want 1", len(camps.adopted))
			}
			var blob struct {
				CustomerID string `json:"customerId"`
				Adopted    bool   `json:"adopted"`
			}
			if err := json.Unmarshal(camps.adopted[0].Result, &blob); err != nil {
				t.Fatalf("persisted blob %s: %v", camps.adopted[0].Result, err)
			}
			if !blob.Adopted || blob.CustomerID != "1112223333" {
				t.Errorf("persisted blob = %s, want the provenance kept and adopted:true", camps.adopted[0].Result)
			}
			if len(camps.indexPayloads) != 1 {
				t.Fatalf("co-committed %d index payloads, want 1", len(camps.indexPayloads))
			}
			if doc := indexedCampaignDoc(t, camps.indexPayloads[0]); !doc.Adopted {
				t.Errorf("indexed campaign doc adopted = false, want true: %s", camps.indexPayloads[0])
			}
		})
	}
}

// A blob that is not a JSON object is an adapter contract violation: refused, never bound with
// the provenance overwritten.
func TestAdoptCampaign_ANonObjectProvenanceBlobIsRefusedAndNothingIsBound(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{
		ID: "1234567890", Name: "Adopted", Variant: model.VariantDefault,
		Result: json.RawMessage(`["1112223333"]`),
	}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)
	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var ise *briefs.InternalServerError
	if !errors.As(err, &ise) {
		t.Fatalf("err = %T %v, want a 500", err, err)
	}
	if len(camps.adopted) != 0 {
		t.Errorf("bound %d rows despite an unusable provenance blob", len(camps.adopted))
	}
}

// GET reads the flag off the stored row; a created campaign carries no flag at all, in the
// response or the index document.
func TestCampaignResult_AdoptedFlagIsTrueOnlyForAdoptedRows(t *testing.T) {
	s, _ := newToggleService(adoptedRow(model.ProviderRedditAds, model.CampaignStatusCreated), &stubToggler{})
	got, err := s.GetCampaign(context.Background(), &briefs.GetCampaignPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1"})
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if got.Adopted == nil || !*got.Adopted {
		t.Errorf("GET adopted = %v, want true", got.Adopted)
	}

	created := &model.Campaign{
		ID: "c2", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_d", Status: model.CampaignStatusCreated, Version: 1,
		Result: json.RawMessage(`{"accountId":"a2_x","adGroupId":"g","adId":"a"}`),
	}
	res := campaignResult(created)
	if res.Adopted != nil {
		t.Errorf("created campaign adopted = %v, want absent", *res.Adopted)
	}
	raw, _ := json.Marshal(campaignDoc(res))
	if strings.Contains(string(raw), `"adopted"`) {
		t.Errorf("created campaign's index doc carries adopted: %s", raw)
	}
	raw, _ = json.Marshal(campaignDoc(campaignResult(adoptedRow(model.ProviderRedditAds, model.CampaignStatusCreated))))
	if !strings.Contains(string(raw), `"adopted":true`) {
		t.Errorf("adopted campaign's index doc lacks adopted:true: %s", raw)
	}
}

// RESUME of an adopted campaign is refused with its own reason; the platform refused it locally
// (no serving children recorded), and nothing is persisted.
func TestToggleCampaignStatus_ResumeOfAnAdoptedCampaignHasItsOwnReason(t *testing.T) {
	tog := &stubToggler{err: fmt.Errorf("%w: reddit campaign t3_c cannot be activated because it was ADOPTED", ErrCampaignNotProvisioned)}
	s, camps := newToggleService(adoptedRow(model.ProviderRedditAds, model.CampaignRunPaused), tog)
	im := "3"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunActive,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %T %v, want a 409", err, err)
	}
	if conflict.Code != "409" || conflict.Reason == nil || *conflict.Reason != "campaign_adopted" {
		t.Errorf("409 = code %q reason %v, want reason campaign_adopted", conflict.Code, conflict.Reason)
	}
	// Fixed text: the dispatcher's sentence (which names the upstream id) never reaches it.
	if strings.Contains(conflict.Message, "t3_c") {
		t.Errorf("the message carries upstream data: %q", conflict.Message)
	}
	if camps.replaced != nil {
		t.Errorf("the row was persisted after a refused resume: %+v", camps.replaced)
	}
}

// A genuinely unprovisioned (non-adopted) campaign keeps the old, reason-less 409.
func TestToggleCampaignStatus_ResumeOfAnUnprovisionedCampaignKeepsTheGeneralConflict(t *testing.T) {
	row := adoptedRow(model.ProviderRedditAds, model.CampaignRunPaused)
	row.Result = json.RawMessage(`{"accountId":"a2_x"}`)
	tog := &stubToggler{err: fmt.Errorf("%w: no ad group recorded", ErrCampaignNotProvisioned)}
	s, _ := newToggleService(row, tog)
	im := "3"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunActive,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %T %v, want a 409", err, err)
	}
	if conflict.Reason != nil {
		t.Errorf("reason = %q, want absent on the general not-provisioned 409", *conflict.Reason)
	}
	if !strings.HasPrefix(conflict.Message, "campaign is not fully provisioned") {
		t.Errorf("message = %q, want the unchanged general text", conflict.Message)
	}
}

// PAUSE of an adopted campaign still works, and a not-provisioned refusal of a pause is some
// other cause — it keeps the general answer rather than claiming adoption is why.
func TestToggleCampaignStatus_PauseOfAnAdoptedCampaignStillWorks(t *testing.T) {
	tog := &stubToggler{}
	s, camps := newToggleService(adoptedRow(model.ProviderRedditAds, model.CampaignStatusCreated), tog)
	im := "3"
	res, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})
	if err != nil {
		t.Fatalf("pause of an adopted campaign: %v", err)
	}
	if res.Status != model.CampaignRunPaused || camps.replaced == nil {
		t.Errorf("pause not persisted: status %q, replaced %+v", res.Status, camps.replaced)
	}
	if res.Adopted == nil || !*res.Adopted {
		t.Errorf("the pause response lost the adopted flag: %v", res.Adopted)
	}

	tog = &stubToggler{err: fmt.Errorf("%w: orphan ad", ErrCampaignNotProvisioned)}
	s, _ = newToggleService(adoptedRow(model.ProviderRedditAds, model.CampaignStatusCreated), tog)
	_, err = s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) || conflict.Reason != nil {
		t.Errorf("a refused PAUSE of an adopted campaign = %v, want the reason-less general 409", err)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The asset id is sent in upper case: the snapshot must record the canonical spelling.
const msLogoExtra = `,"logoAssetId":"6F1C2A3B-4D5E-4F60-8A7B-9C0D1E2F3A4B","logoBusinessName":" CNCF "`

type msLogoResultBlob struct {
	MediaID     string   `json:"logoMediaId"`
	ExtensionID string   `json:"logoAdExtensionId"`
	Steps       []string `json:"steps"`
}

func ownedLogoAsset(t *testing.T, mime string, w, h int) *scopedCreativeAssets {
	brief := testBrief()
	return &scopedCreativeAssets{
		projectID: brief.ProjectID, briefID: brief.ID, assetID: msAssetID,
		asset: &model.CreativeAsset{ID: msAssetID, MimeType: mime, Bytes: msTestPNG(t, w, h)},
	}
}

func newMSLogoDispatcher(t *testing.T, api *msTextAPI, assets creativeAssetReader) *MicrosoftDispatcher {
	d := newMSTextDispatcher(t, api)
	d.SetCreativeAssetRepo(assets)
	return d
}

// Off (the default, and any value but "true"): either field is a synchronous 400 naming the flag,
// and Dispatch refuses it again, pre-create, before any Microsoft call. Empty and null are absent.
func TestMicrosoftLogo_GateOffRefusesBothPaths(t *testing.T) {
	api := &msTextAPI{}
	assets := ownedLogoAsset(t, model.MimeTypePNG, 256, 256)
	d := newMSLogoDispatcher(t, api, assets)
	for _, v := range []string{"", "True", "1"} {
		t.Setenv(constants.EnvMicrosoftLogoExtensionEnabled, v)
		for _, extra := range []string{msLogoExtra, `,"logoBusinessName":"CNCF"`} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(extra))
			requireConfigInvalid(t, err, "Microsoft logo extensions are disabled")
			requireConfigInvalid(t, err, constants.EnvMicrosoftLogoExtensionEnabled)
		}
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msLogoExtra))
	requirePreCreate(t, err)
	requireConfigInvalid(t, err, "microsoftConfig.logoAssetId: Microsoft logo extensions are disabled")
	if api.total() != 0 || assets.loads() != 0 {
		t.Errorf("%d Microsoft calls and %d asset loads before the gate refused; want 0", api.total(), assets.loads())
	}
	for _, extra := range []string{``, `,"logoAssetId":"","logoBusinessName":""`, `,"logoAssetId":null,"logoBusinessName":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type or a config-level rule breach is the synchronous 400 naming the field, and
// Dispatch refuses the same input before any Microsoft call or asset load.
func TestMicrosoftLogo_PreflightRules(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftLogoExtensionEnabled, "true")
	api := &msTextAPI{}
	assets := ownedLogoAsset(t, model.MimeTypePNG, 256, 256)
	d := newMSLogoDispatcher(t, api, assets)
	const id = `"logoAssetId":"` + msAssetID + `"`
	for extra, want := range map[string]string{
		`,"logoAssetId":42,"logoBusinessName":"CNCF"`:                        "microsoftConfig.logoAssetId: must be a string",
		`,` + id + `,"LOGOBUSINESSNAME":["CNCF"]`:                            "microsoftConfig.logoBusinessName: must be a string",
		`,"logoAssetId":"not-a-uuid","logoBusinessName":"CNCF"`:              "microsoftConfig.logoAssetId: is not a valid asset id",
		`,"logoBusinessName":"CNCF"`:                                         "microsoftConfig.logoAssetId: is required with logoBusinessName",
		`,` + id:                                                             "microsoftConfig.logoBusinessName: is required with logoAssetId",
		`,` + id + `,"logoBusinessName":"  "`:                                "microsoftConfig.logoBusinessName: is required with logoAssetId",
		`,` + id + `,"logoBusinessName":"` + strings.Repeat("a", 26) + `"`:   "microsoftConfig.logoBusinessName: the business name is 26 characters; the limit is 25",
		`,` + id + `,"logoBusinessName":"CNCF\n"`:                            "microsoftConfig.logoBusinessName: the business name contains a control",
		`,"budget":"x","logoAssetId":"not-a-uuid","logoBusinessName":"CNCF"`: "microsoftConfig.logoAssetId: is not a valid asset id",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(extra))
		requireConfigInvalid(t, err, want)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(`,`+id))
	requirePreCreate(t, err)
	requireConfigInvalid(t, err, "logoBusinessName: is required")
	if api.total() != 0 || assets.loads() != 0 {
		t.Errorf("%d Microsoft calls and %d asset loads before the rules refused; want 0", api.total(), assets.loads())
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(msLogoExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid: PreflightCreate = %v, want not applicable", err)
	}
}

// On: an asset that is missing, not square, too small, too large in bytes or not a PNG/JPEG
// (video/mp4 included) fails the job pre-create, before any Microsoft call.
func TestMicrosoftLogo_AssetRefusedBeforeAnyMicrosoftCall(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftLogoExtensionEnabled, "true")
	oversized := ownedLogoAsset(t, model.MimeTypePNG, 256, 256)
	oversized.sizeOverride = 76801
	foreign := ownedLogoAsset(t, model.MimeTypePNG, 256, 256)
	foreign.briefID = msOtherBriefID
	for name, tc := range map[string]struct {
		assets *scopedCreativeAssets
		want   string
	}{
		"not square": {ownedLogoAsset(t, model.MimeTypePNG, 703, 368), "703x368; a logo must be square (1:1)"},
		"too small":  {ownedLogoAsset(t, model.MimeTypePNG, 100, 100), "a logo must be at least 128x128"},
		"video":      {ownedLogoAsset(t, "video/mp4", 256, 256), "only PNG and JPEG images are supported"},
		"oversized":  {oversized, "76801 bytes; Microsoft Advertising logo uploads accept at most 76800"},
		"foreign":    {foreign, "does not exist for this brief"},
	} {
		t.Run(name, func(t *testing.T) {
			api := &msTextAPI{}
			d := newMSLogoDispatcher(t, api, tc.assets)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msLogoExtra))
			requirePreCreate(t, err)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if api.total() != 0 {
				t.Errorf("%d Microsoft calls; want 0", api.total())
			}
		})
	}
}

// On: the logo is uploaded once as an Image1x1, created as a LogoAdExtension naming that media and
// the trimmed business name, and associated with the confirmed campaign; the ids are recorded. A
// retry on the reused campaign, which now has a logo, uploads and adds nothing.
func TestMicrosoftLogo_DispatchAttachesAndRetryAddsNone(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftLogoExtensionEnabled, "true")
	api := &msTextAPI{}
	d := newMSLogoDispatcher(t, api, ownedLogoAsset(t, model.MimeTypePNG, 256, 256))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msLogoExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	var blob msLogoResultBlob
	if err := json.Unmarshal(camp.Result, &blob); err != nil || blob.MediaID != "5551" || blob.ExtensionID != "9001" {
		t.Fatalf("result = %+v (%v)", blob, err)
	}
	if api.count("Media", `"MediaType":"Image1x1"`) != 1 {
		t.Error("the logo must be uploaded once as an Image1x1")
	}
	if api.count("AdExtensions", `{"Type":"LogoAdExtension","BusinessLogo":5551,"BusinessName":"CNCF"}`) != 1 {
		t.Error("one LogoAdExtension must name the uploaded media and the trimmed business name")
	}
	if api.count("AdExtensionsAssociations/Query", `"AdExtensionType":"LogoAdExtension"`) != 1 {
		t.Error("the campaign must be checked for an existing logo first")
	}
	var snap microsoftConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil || snap.LogoAssetID != msAssetID || snap.LogoBusinessName != "CNCF" {
		t.Errorf("snapshot = %+v (%v)", snap, err)
	}
	media, exts := api.count("Media", ""), api.count("AdExtensions", "")
	camp2, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msLogoExtra))
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if api.count("Media", "") != media || api.count("AdExtensions", "") != exts || camp2.Status != campaignStatusCreated {
		t.Errorf("retry uploaded %d and created %d, status %q; want none and created",
			api.count("Media", "")-media, api.count("AdExtensions", "")-exts, camp2.Status)
	}
	var blob2 msLogoResultBlob
	_ = json.Unmarshal(camp2.Result, &blob2)
	if blob2.MediaID != "" || blob2.ExtensionID != "9001" || !strings.Contains(strings.Join(blob2.Steps, "\n"), "Logo extension already associated with the campaign: 9001") {
		t.Errorf("retry result = %+v", blob2)
	}
}

// A logo that does not land degrades the campaign rather than failing the job, with a step that
// says whether to verify (UNCONFIRMED) or re-attach (NOT attached).
func TestMicrosoftLogo_ShortfallDegrades(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftLogoExtensionEnabled, "true")
	for name, tc := range map[string]struct {
		path, body, step string
		status           int
	}{
		"extension 5xx": {"AdExtensions", `{}`, "Logo extension UNCONFIRMED", 502},
		"upload 400":    {"Media", `{"Code":1001,"ErrorCode":"InvalidRequest"}`, "Logo extension NOT attached", 400},
	} {
		t.Run(name, func(t *testing.T) {
			api := &msTextAPI{failAt: map[string]struct {
				status int
				body   string
			}{tc.path: {tc.status, tc.body}}}
			d := newMSLogoDispatcher(t, api, ownedLogoAsset(t, model.MimeTypePNG, 256, 256))
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msLogoExtra))
			if err != nil {
				t.Fatalf("the campaign exists, so the job must not fail: %v", err)
			}
			if camp.Status != campaignStatusCreatedDegraded {
				t.Fatalf("status = %q, want created_degraded", camp.Status)
			}
			var blob msLogoResultBlob
			_ = json.Unmarshal(camp.Result, &blob)
			if steps := strings.Join(blob.Steps, "\n"); !strings.Contains(steps, tc.step) {
				t.Errorf("steps:\n%s", steps)
			}
		})
	}
}

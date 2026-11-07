// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const msTextExtExtra = `,"sitelinks":[{"text":"Schedule","description1":"Five days of sessions","description2":"Keynotes and labs","finalUrl":"https://events.example/kc/schedule?utm_source=x&token=secret"},{"text":"Register","finalUrl":"https://events.example/kc/register"}],"callouts":["Early Bird Pricing","300+ Sessions"]`

// msTextAPI fakes the campaign tree and the extension steps. It is STATEFUL per extension type:
// once an extension of a type is associated, the type's lookup reports it, which is what a retry
// against the real service would see.
type msTextAPI struct {
	mu       sync.Mutex
	calls    []string
	bodies   []string
	next     int
	attached map[string][]string // AdExtensionType -> associated ids
	pending  map[string]string   // created id -> AdExtensionType
	failAt   map[string]struct {
		status int
		body   string
	}
}

func (a *msTextAPI) count(path, contains string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for i, c := range a.calls {
		if c == path && strings.Contains(a.bodies[i], contains) {
			n++
		}
	}
	return n
}

func (a *msTextAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func newMSTextDispatcher(t *testing.T, api *msTextAPI) *MicrosoftDispatcher {
	t.Helper()
	api.attached, api.pending = map[string][]string{}, map[string]string{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		p := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		api.mu.Lock()
		defer api.mu.Unlock()
		api.calls = append(api.calls, p)
		api.bodies = append(api.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if f, ok := api.failAt[p]; ok {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.body)
			return
		}
		switch p {
		case "Campaigns/QueryByAccountId":
			if len(api.attached) > 0 || api.next > 0 {
				// A retry: the campaign the first dispatch created is found by name.
				_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"Name":"`+lastCampaignName(api)+`"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case "AdGroups/QueryByCampaignId":
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case "Ads/QueryByAdGroupId":
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case "Campaigns":
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case "AdGroups":
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case "Ads":
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case "AdExtensionsAssociations/Query":
			var q struct{ AdExtensionType string }
			_ = json.Unmarshal(b, &q)
			if ids := api.attached[q.AdExtensionType]; len(ids) > 0 {
				assocs := make([]string, len(ids))
				for i, id := range ids {
					assocs[i] = fmt.Sprintf(`{"AdExtension":{"Id":%s,"Type":%q},"AssociationType":"Campaign","EntityId":321}`, id, q.AdExtensionType)
				}
				_, _ = fmt.Fprintf(w, `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[%s]}]}`, strings.Join(assocs, ","))
				return
			}
			_, _ = io.WriteString(w, `{"AdExtensionAssociationCollection":[null]}`)
		case "AdExtensions":
			api.next++
			id := fmt.Sprint(9000 + api.next)
			typ := "CalloutAdExtension"
			if strings.Contains(body, "SitelinkAdExtension") {
				typ = "SitelinkAdExtension"
			}
			api.pending[id] = typ
			_, _ = fmt.Fprintf(w, `{"AdExtensionIdentities":[{"Id":%s,"Version":1}],"NestedPartialErrors":[]}`, id)
		case "AdExtensionsAssociations/Set":
			var s struct {
				AdExtensionIdToEntityIdAssociations []struct{ AdExtensionId json.Number }
			}
			_ = json.Unmarshal(b, &s)
			if len(s.AdExtensionIdToEntityIdAssociations) == 1 {
				id := s.AdExtensionIdToEntityIdAssociations[0].AdExtensionId.String()
				typ := api.pending[id]
				api.attached[typ] = append(api.attached[typ], id)
			}
			_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
}

// lastCampaignName returns the Name the first AddCampaigns sent (caller holds api.mu).
func lastCampaignName(api *msTextAPI) string {
	for i, c := range api.calls {
		if c == "Campaigns" {
			var req struct{ Campaigns []struct{ Name string } }
			if json.Unmarshal([]byte(api.bodies[i]), &req) == nil && len(req.Campaigns) == 1 {
				return req.Campaigns[0].Name
			}
		}
	}
	return ""
}

func msTextCfg(extra string) json.RawMessage {
	return json.RawMessage(`{"microsoftConfig":{"budget":50` + extra + `}}`)
}

type msTextResultBlob struct {
	SitelinkAdExtensionIDs []string `json:"sitelinkAdExtensionIds"`
	CalloutAdExtensionIDs  []string `json:"calloutAdExtensionIds"`
	Steps                  []string `json:"steps"`
}

func decodeMSTextResult(t *testing.T, camp *model.Campaign) msTextResultBlob {
	t.Helper()
	var b msTextResultBlob
	if err := json.Unmarshal(camp.Result, &b); err != nil {
		t.Fatalf("result blob: %v", err)
	}
	return b
}

// Off (the default): either list is a synchronous 400 naming the flag, and Dispatch refuses it
// again, pre-create, before any Microsoft call.
func TestMicrosoftTextExtensions_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "")
	api := &msTextAPI{}
	d := newMSTextDispatcher(t, api)
	for _, extra := range []string{msTextExtExtra, `,"sitelinks":[{"text":"x","finalUrl":"https://x.example"}]`, `,"callouts":["a","b"]`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(extra))
		requireConfigInvalid(t, err, constants.EnvMicrosoftAdExtensionsEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftAdExtensionsEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := api.total(); n != 0 {
		t.Errorf("%d Microsoft API calls before the gate refused; want 0", n)
	}
	// Nothing requested is not gated.
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(`,"sitelinks":[],"callouts":null`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("empty lists must not trip the gate, got %v", err)
	}
}

// Only the exact value "true" opens the gate.
func TestMicrosoftTextExtensions_GateFailsClosedOnTypo(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "TRUE")
	d := newMSTextDispatcher(t, &msTextAPI{})
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra))
	requireConfigInvalid(t, err, constants.EnvMicrosoftAdExtensionsEnabled)
}

// An unrelated mistyped field must not let a gated or invalid list through preflight.
func TestMicrosoftTextExtensions_PreflightRefusesDespiteUnrelatedDecodeError(t *testing.T) {
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":"bad","callouts":["only one"]}}`)
	t.Run("gate off", func(t *testing.T) {
		t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "")
		err := newMSTextDispatcher(t, &msTextAPI{}).PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, cfg)
		requireConfigInvalid(t, err, constants.EnvMicrosoftAdExtensionsEnabled)
	})
	t.Run("gate on", func(t *testing.T) {
		t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "true")
		err := newMSTextDispatcher(t, &msTextAPI{}).PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, cfg)
		requireConfigInvalid(t, err, "microsoftConfig.callouts: at least 2")
	})
}

// On: a valid config passes preflight; each rule breach is a synchronous 400 naming the field.
func TestMicrosoftTextExtensions_PreflightRules(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "true")
	d := newMSTextDispatcher(t, &msTextAPI{})
	ctx := context.Background()
	if err := d.PreflightCreate(ctx, "cncf", model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("a valid config must pass preflight, got %v", err)
	}
	cases := map[string]struct{ extra, want string }{
		"one callout":                 {`,"callouts":["x"]`, "microsoftConfig.callouts: at least 2"},
		"sitelink without url":        {`,"sitelinks":[{"text":"x"}]`, "microsoftConfig.sitelinks: [0].finalUrl is required"},
		"lone description":            {`,"sitelinks":[{"text":"x","description1":"y","finalUrl":"https://x.example"}]`, "microsoftConfig.sitelinks: [0]: description1 and description2"},
		"sitelinks not an array":      {`,"sitelinks":{"text":"x"}`, "microsoftConfig.sitelinks: must be an array of objects"},
		"sitelink field not a string": {`,"sitelinks":[{"text":1,"finalUrl":"https://x.example"}]`, "microsoftConfig.sitelinks: must be an array of objects"},
		"callouts not strings":        {`,"callouts":[1,2]`, "microsoftConfig.callouts: must be an array of strings"},
		"case-variant key wrong type": {`,"Callouts":"a"`, "microsoftConfig.callouts: must be an array of strings"},
		"case-variant sitelinks":      {`,"SITELINKS":[1]`, "microsoftConfig.sitelinks: must be an array of objects"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := d.PreflightCreate(ctx, "cncf", model.ProviderMicrosoftAds, msTextCfg(tc.extra))
			requireConfigInvalid(t, err, tc.want)
		})
	}
	if err := d.PreflightCreate(ctx, "cncf", model.ProviderMicrosoftAds, msTextCfg(`,"sitelinks":null`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("null must read as absent, got %v", err)
	}
}

// On: Dispatch re-checks before any Microsoft call, so a job that skipped preflight still fails
// pre-create with nothing upstream.
func TestMicrosoftTextExtensions_DispatchRefusesInvalidBeforeAnyCall(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "true")
	api := &msTextAPI{}
	d := newMSTextDispatcher(t, api)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(`,"callouts":["x"]`))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), "microsoftConfig.callouts") {
		t.Errorf("refusal must name the field: %v", err)
	}
	if n := api.total(); n != 0 {
		t.Errorf("%d calls before the refusal", n)
	}
}

// On: the extensions are created and associated after the campaign, recorded on the result, and
// the snapshot reduces the destination URL; a retry that reuses the campaign adds none again.
func TestMicrosoftTextExtensions_DispatchAttachesAndRetryDoesNotDouble(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "true")
	api := &msTextAPI{}
	d := newMSTextDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	blob := decodeMSTextResult(t, camp)
	if strings.Join(blob.SitelinkAdExtensionIDs, ",") != "9001,9002" || strings.Join(blob.CalloutAdExtensionIDs, ",") != "9003,9004" {
		t.Errorf("ids = %+v", blob)
	}
	if api.count("AdExtensions", `"DisplayText":"Schedule"`) != 1 || api.count("AdExtensions", `"Text":"300+ Sessions"`) != 1 {
		t.Error("each sitelink and callout must be sent once")
	}
	if api.count("AdExtensions", `token=secret`) != 1 {
		t.Error("the FULL sitelink URL must reach Microsoft")
	}
	var snap microsoftConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Sitelinks) != 2 || len(snap.Callouts) != 2 || strings.Contains(string(camp.ConfigSnapshot), "token=secret") {
		t.Errorf("snapshot must keep both lists with the URL reduced: %s", camp.ConfigSnapshot)
	}

	before := api.count("AdExtensions", "")
	camp2, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra))
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if n := api.count("AdExtensions", "") - before; n != 0 {
		t.Errorf("a retry on the reused campaign created %d more extensions", n)
	}
	if camp2.Status != campaignStatusCreated {
		t.Errorf("an already-attached retry is not degraded, got %q", camp2.Status)
	}
	if steps := strings.Join(decodeMSTextResult(t, camp2).Steps, "\n"); !strings.Contains(steps, "Sitelink extensions already associated") || !strings.Contains(steps, "Callout extensions already associated") {
		t.Errorf("the retry must say both types were already there:\n%s", steps)
	}
}

// An extension failure degrades the campaign rather than failing the job.
func TestMicrosoftTextExtensions_FailureDegrades(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "true")
	for name, f := range map[string]struct {
		path, step string
		status     int
		body       string
	}{
		"create ambiguous":  {"AdExtensions", "UNCONFIRMED", 502, `{"Message":"` + msLeakText + `"}`},
		"lookup failed":     {"AdExtensionsAssociations/Query", "NOT attached", 503, `{"Message":"` + msLeakText + `"}`},
		"association fails": {"AdExtensionsAssociations/Set", "NOT attached", 400, `{"Code":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			api := &msTextAPI{failAt: map[string]struct {
				status int
				body   string
			}{f.path: {f.status, f.body}}}
			d := newMSTextDispatcher(t, api)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra))
			if err != nil {
				t.Fatalf("the campaign exists, so the job must not fail: %v", err)
			}
			if camp.PlatformCampaignID != "321" || camp.Status != campaignStatusCreatedDegraded {
				t.Errorf("campaign = %q status %q, want 321 created_degraded", camp.PlatformCampaignID, camp.Status)
			}
			if steps := strings.Join(decodeMSTextResult(t, camp).Steps, "\n"); !strings.Contains(steps, f.step) {
				t.Errorf("steps must record %q:\n%s", f.step, steps)
			}
			if strings.Contains(string(camp.Result), msLeakText) {
				t.Error("upstream message text leaked into the persisted result")
			}
		})
	}
}

// An UNCONFIRMED campaign create attaches nothing: there is no confirmed campaign.
func TestMicrosoftTextExtensions_UnconfirmedCreateSkipsThem(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdExtensionsEnabled, "true")
	api := &msTextAPI{failAt: map[string]struct {
		status int
		body   string
	}{"Campaigns": {502, `{}`}}}
	d := newMSTextDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msTextExtExtra))
	if err == nil || camp == nil {
		t.Fatalf("want an UNCONFIRMED partial, got %v, %v", camp, err)
	}
	if n := api.count("AdExtensions", "") + api.count("AdExtensionsAssociations/Query", ""); n != 0 {
		t.Errorf("%d extension calls on an unconfirmed campaign", n)
	}
}

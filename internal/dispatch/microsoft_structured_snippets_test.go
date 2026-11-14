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

const msSnippetExtra = `,"structuredSnippets":[{"header":"courses","values":["Kubernetes","Observability","Security"]},{"header":"Types","values":["Keynotes","Talks","Labs"]}]`

type msSnippetResultBlob struct {
	IDs   []string `json:"structuredSnippetAdExtensionIds"`
	Steps []string `json:"steps"`
}

// Off (the default, and any value but "true"): the list is a synchronous 400 naming the flag, and
// Dispatch refuses it again, pre-create, before any Microsoft call. Empty and null are absent.
func TestMicrosoftStructuredSnippets_GateOffRefusesBothPaths(t *testing.T) {
	api := &msTextAPI{}
	d := newMSTextDispatcher(t, api)
	for _, v := range []string{"", "True"} {
		t.Setenv(constants.EnvMicrosoftStructuredSnippetsEnabled, v)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(msSnippetExtra))
		requireConfigInvalid(t, err, "microsoftConfig.structuredSnippets: Microsoft structured snippets are disabled")
		requireConfigInvalid(t, err, constants.EnvMicrosoftStructuredSnippetsEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msSnippetExtra))
	requirePreCreate(t, err)
	if api.total() != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", api.total())
	}
	for _, extra := range []string{``, `,"structuredSnippets":[]`, `,"structuredSnippets":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type or a rule breach is the synchronous 400 naming the field, and Dispatch refuses
// the same input before any Microsoft call.
func TestMicrosoftStructuredSnippets_PreflightRules(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftStructuredSnippetsEnabled, "true")
	api := &msTextAPI{}
	d := newMSTextDispatcher(t, api)
	for extra, want := range map[string]string{
		`,"structuredSnippets":{"header":"Types"}`:                               "microsoftConfig.structuredSnippets: must be an array of objects",
		`,"STRUCTUREDSNIPPETS":[{"header":"Types","values":"a"}]`:                "microsoftConfig.structuredSnippets: must be an array of objects",
		`,"structuredSnippets":[{"header":"Speakers","values":["a","b","c"]}]`:   "microsoftConfig.structuredSnippets: [0].header must be one of",
		`,"structuredSnippets":[{"header":"Types","values":["a","b"]}]`:          "microsoftConfig.structuredSnippets: [0].values must hold between 3 and 10",
		`,"budget":"x","structuredSnippets":[{"header":"Types","values":["a"]}]`: "microsoftConfig.structuredSnippets: [0].values must hold",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(extra))
		requireConfigInvalid(t, err, want)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(`,"structuredSnippets":[{"header":"Speakers","values":["a","b","c"]}]`))
	requirePreCreate(t, err)
	if api.total() != 0 {
		t.Errorf("%d Microsoft calls before the rules refused; want 0", api.total())
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msTextCfg(msSnippetExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid: PreflightCreate = %v, want not applicable", err)
	}
}

// On: each snippet is created and associated once on the confirmed campaign, with the header in
// Microsoft's spelling; a retry on the reused campaign adds none.
func TestMicrosoftStructuredSnippets_DispatchAttachesAndRetryDoesNotDouble(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftStructuredSnippetsEnabled, "true")
	api := &msTextAPI{}
	d := newMSTextDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msSnippetExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	var blob msSnippetResultBlob
	if err := json.Unmarshal(camp.Result, &blob); err != nil || strings.Join(blob.IDs, ",") != "9001,9002" {
		t.Fatalf("ids = %+v (%v)", blob, err)
	}
	if api.count("AdExtensions", `"Header":"Courses","Values":["Kubernetes","Observability","Security"]`) != 1 {
		t.Error("the Courses snippet must be sent once, in Microsoft's spelling")
	}
	var snap microsoftConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil || len(snap.StructuredSnippets) != 2 || len(snap.StructuredSnippets[1].Values) != 3 {
		t.Errorf("snapshot must keep the list: %s (%v)", camp.ConfigSnapshot, err)
	}
	before := api.count("AdExtensions", "")
	camp2, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msSnippetExtra))
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if n := api.count("AdExtensions", "") - before; n != 0 || camp2.Status != campaignStatusCreated {
		t.Errorf("retry created %d snippets, status %q", n, camp2.Status)
	}
}

// A snippet that does not land degrades the campaign rather than failing the job.
func TestMicrosoftStructuredSnippets_FailureDegrades(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftStructuredSnippetsEnabled, "true")
	api := &msTextAPI{failAt: map[string]struct {
		status int
		body   string
	}{"AdExtensions": {502, `{}`}}}
	d := newMSTextDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msTextCfg(msSnippetExtra))
	if err != nil {
		t.Fatalf("the campaign exists, so the job must not fail: %v", err)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Fatalf("status = %q, want created_degraded", camp.Status)
	}
	var blob msSnippetResultBlob
	_ = json.Unmarshal(camp.Result, &blob)
	if steps := strings.Join(blob.Steps, "\n"); !strings.Contains(steps, "Structured snippet extension 0 UNCONFIRMED") {
		t.Errorf("steps:\n%s", steps)
	}
}

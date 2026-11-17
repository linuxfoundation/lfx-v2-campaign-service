// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goahttp "goa.design/goa/v3/http"
	"goa.design/goa/v3/security"

	briefssrv "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// systemAccountAdopter is a LinkedIn adopter that records the adoption account its context carried
// and answers with a row recording that account, as the real dispatcher does.
type systemAccountAdopter struct {
	err        error
	calls      int
	gotAccount string
	listRows   []model.AdoptableCampaign
	listErr    error
	listGot    string
}

func (systemAccountAdopter) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch must never be called on the adoption path")
}

func (d *systemAccountAdopter) LookupCampaign(ctx context.Context, _ string, _ model.Provider, id string) (*model.PlatformCampaignRef, error) {
	d.calls++
	d.gotAccount = model.AdoptionAccount(ctx)
	if d.err != nil {
		return nil, d.err
	}
	account := d.gotAccount
	if account == "" {
		account = "123456789"
	}
	return &model.PlatformCampaignRef{ID: id, Name: "Hand built", Variant: model.VariantDefault,
		Result: json.RawMessage(`{"accountId":"` + account + `","adopted":true}`)}, nil
}

func (d *systemAccountAdopter) ListAdoptableCampaigns(_ context.Context, _ string, _ model.Provider, accountID string) ([]model.AdoptableCampaign, error) {
	d.listGot = accountID
	return d.listRows, d.listErr
}

func systemAdoptPayload(account string) *briefs.AdoptCampaignPayload {
	return &briefs.AdoptCampaignPayload{ProjectID: "cncf", BriefID: "b1",
		Platform: string(model.ProviderLinkedInAds), PlatformCampaignID: "312", AccountID: &account}
}

// TestAdoptCampaign_AccountIDIsOpsOnlyAndFlagGated pins #581 PR 5's gates: account_id is
// linkedin-only, needs LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED and global_marketing_ops, and every
// refusal is made before LinkedIn is contacted.
func TestAdoptCampaign_AccountIDIsOpsOnlyAndFlagGated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flag      string
		platform  model.Provider
		account   string
		checker   *fakeMarketingOps
		want      string
		wantCalls int // checker calls
	}{
		{"flag off", "", model.ProviderLinkedInAds, "500928401", &fakeMarketingOps{allow: true}, "400", 0},
		{"flag not exactly true", "TRUE", model.ProviderLinkedInAds, "500928401", &fakeMarketingOps{allow: true}, "400", 0},
		{"another platform", "true", model.ProviderGoogleAds, "500928401", &fakeMarketingOps{allow: true}, "400", 0},
		{"blank account", "true", model.ProviderLinkedInAds, "  ", &fakeMarketingOps{allow: true}, "400", 0},
		{"an executive director", "true", model.ProviderLinkedInAds, "500928401", &fakeMarketingOps{}, "403", 1},
		{"the check failed", "true", model.ProviderLinkedInAds, "500928401", &fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, "503", 1},
		{"marketing ops", "true", model.ProviderLinkedInAds, "500928401", &fakeMarketingOps{allow: true}, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, tc.flag)
			disp := &systemAccountAdopter{}
			s, camps := newAdoptService(t, tc.platform, disp)
			s.SetMarketingOpsChecker(tc.checker)
			p := systemAdoptPayload(tc.account)
			p.Platform = string(tc.platform)
			_, err := s.AdoptCampaign(ctxWithActor(testActor), p)
			switch tc.want {
			case "":
				if err != nil {
					t.Fatalf("AdoptCampaign = %v", err)
				}
				if disp.gotAccount != "500928401" {
					t.Fatalf("lookup carried account %q, want 500928401", disp.gotAccount)
				}
			case "400":
				var e *briefs.BadRequestError
				if !errors.As(err, &e) {
					t.Fatalf("err = %T %v, want 400", err, err)
				}
			case "403":
				var e *briefs.ForbiddenError
				if !errors.As(err, &e) || !strings.Contains(e.Message, "global_marketing_ops") {
					t.Fatalf("err = %T %v, want 403", err, err)
				}
			case "503":
				var e *briefs.ConnServiceUnavailableError
				if !errors.As(err, &e) {
					t.Fatalf("err = %T %v, want 503", err, err)
				}
			}
			if tc.want != "" && (disp.calls != 0 || len(camps.adopted) != 0) {
				t.Fatalf("a refused adoption reached LinkedIn %d time(s) / bound %d row(s)", disp.calls, len(camps.adopted))
			}
			if got := len(tc.checker.calls); got != tc.wantCalls {
				t.Fatalf("checker called %d time(s), want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestAdoptCampaign_WithoutAccountIDIsUnchanged: no account_id means no gate and no account in the
// lookup's context, exactly as before #581.
func TestAdoptCampaign_WithoutAccountIDIsUnchanged(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	disp := &systemAccountAdopter{}
	s, _ := newAdoptService(t, model.ProviderLinkedInAds, disp)
	checker := &fakeMarketingOps{}
	s.SetMarketingOpsChecker(checker)
	p := systemAdoptPayload("")
	p.AccountID = nil
	if _, err := s.AdoptCampaign(ctxWithActor(testActor), p); err != nil {
		t.Fatalf("AdoptCampaign: %v", err)
	}
	if disp.gotAccount != "" || len(checker.calls) != 0 {
		t.Fatalf("account %q / %d checks on an ordinary adoption", disp.gotAccount, len(checker.calls))
	}
}

// TestAdoptCampaign_SystemAccountAdoptionIsIdempotent: repeating the same adoption returns the
// bound row without contacting LinkedIn; a different campaign or account on the slot stays a 409.
func TestAdoptCampaign_SystemAccountAdoptionIsIdempotent(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	disp := &systemAccountAdopter{}
	s, camps := newAdoptService(t, model.ProviderLinkedInAds, disp)
	s.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	ctx := ctxWithActor(testActor)
	first, err := s.AdoptCampaign(ctx, systemAdoptPayload("500928401"))
	if err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	again, err := s.AdoptCampaign(ctx, systemAdoptPayload("500928401"))
	if err != nil {
		t.Fatalf("repeat adoption: %v", err)
	}
	if again.ID != first.ID || disp.calls != 1 || len(camps.adopted) != 1 {
		t.Fatalf("repeat answered %s (first %s), %d lookups, %d rows; want the same row, 1 lookup, 1 row", again.ID, first.ID, disp.calls, len(camps.adopted))
	}
	for name, p := range map[string]*briefs.AdoptCampaignPayload{
		"another account": systemAdoptPayload("538170226"),
		"another campaign": func() *briefs.AdoptCampaignPayload {
			p := systemAdoptPayload("500928401")
			p.PlatformCampaignID = "313"
			return p
		}(),
	} {
		var conflict *briefs.ConflictError
		if _, err := s.AdoptCampaign(ctx, p); !errors.As(err, &conflict) {
			t.Fatalf("%s on an occupied slot = %v, want 409", name, err)
		}
	}
}

// TestAdoptCampaign_SystemAccountLookupErrors maps the refusals only the account_id path reaches.
func TestAdoptCampaign_SystemAccountLookupErrors(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	for name, tc := range map[string]struct {
		err   error
		check func(error) bool
	}{
		"ungranted account": {domain.ErrAccountNotManagedByConnection, func(e error) bool { var x *briefs.BadRequestError; return errors.As(e, &x) }},
		"no system row": {errors.Join(domain.ErrSystemConnectionMissing, domain.ErrNotFound), func(e error) bool {
			var x *briefs.InternalServerError
			return errors.As(e, &x)
		}},
		"broken system row": {errors.Join(domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable), func(e error) bool {
			var x *briefs.InternalServerError
			return errors.As(e, &x)
		}},
		"foreign campaign": {domain.ErrCampaignAccountMismatch, func(e error) bool {
			var x *briefs.ConflictError
			return errors.As(e, &x) && strings.Contains(x.Message, "account_id")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			disp := &systemAccountAdopter{err: tc.err}
			s, camps := newAdoptService(t, model.ProviderLinkedInAds, disp)
			s.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
			_, err := s.AdoptCampaign(ctxWithActor(testActor), systemAdoptPayload("500928401"))
			if !tc.check(err) {
				t.Fatalf("err = %T %v", err, err)
			}
			if len(camps.adopted) != 0 {
				t.Fatal("a refused adoption bound a row")
			}
		})
	}
}

// TestAdoptCampaign_SystemRowDecryptFailureIs500AndLogsNoErrorText pins the account_id path's
// answer to an LF system row that fails authenticated decryption: the same 500 as a broken LF
// row, never the 503 default, and a log line that names the LF row but carries none of the
// decryptor's error text (domain.Encryptor may quote the ciphertext or key it failed on).
// decryptConn wraps ErrCredentialDecryptionFailed, not ErrConnectionNotUsable, so systemScoped
// adds no ErrSystemConnectionNotUsable and the broken-row arm alone would miss it.
func TestAdoptCampaign_SystemRowDecryptFailureIs500AndLogsNoErrorText(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const leaked = "SUPERSECRETCIPHERTEXTBYTES"
	disp := &systemAccountAdopter{err: fmt.Errorf("decrypt linkedin-ads credentials: %w: aesgcm: open failed on %s",
		domain.ErrCredentialDecryptionFailed, leaked)}
	s, camps := newAdoptService(t, model.ProviderLinkedInAds, disp)
	s.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	_, err := s.AdoptCampaign(ctxWithActor(testActor), systemAdoptPayload("500928401"))

	var internal *briefs.InternalServerError
	if !errors.As(err, &internal) {
		t.Fatalf("err = %T %v, want the 500 a broken LF system row gets", err, err)
	}
	if strings.Contains(internal.Message, leaked) {
		t.Errorf("the decryptor's error text reached the response: %q", internal.Message)
	}
	if len(camps.adopted) != 0 {
		t.Fatal("a refused adoption bound a row")
	}
	logged := buf.String()
	if strings.Contains(logged, leaked) {
		t.Errorf("the decryptor's error text reached the log.\nlog: %s", logged)
	}
	if !strings.Contains(logged, "project_id="+model.SystemProjectID) || !strings.Contains(logged, "requested_by_project_id=") {
		t.Errorf("the log must name the LF system row that failed, with the requester beside it.\nlog: %s", logged)
	}
}

// TestListLinkedinAdsAdoptableCampaigns_SystemRowDecryptFailureIs500AndLogsNoErrorText pins the
// candidate list's twin of the adoption arm above. classifyDiscoveryError already answers an
// authenticated-decryption failure with a 500 and a fixed log line; this keeps the two #581
// endpoints from drifting apart.
func TestListLinkedinAdsAdoptableCampaigns_SystemRowDecryptFailureIs500AndLogsNoErrorText(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const leaked = "SUPERSECRETCIPHERTEXTBYTES"
	disp := &systemAccountAdopter{listErr: fmt.Errorf("%w: decrypt linkedin-ads credentials: %w: aesgcm: open failed on %s",
		domain.ErrSystemConnectionOrigin, domain.ErrCredentialDecryptionFailed, leaked)}
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{}}
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(camps, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: disp}))
	_, err := svc.ListLinkedinAdsAdoptableCampaigns(context.Background(), &conn.ListLinkedinAdsAdoptableCampaignsPayload{ProjectID: "cncf", AccountID: "500928401"})

	var internal *conn.InternalServerError
	if !errors.As(err, &internal) {
		t.Fatalf("err = %T %v, want the 500", err, err)
	}
	if strings.Contains(internal.Message, leaked) {
		t.Errorf("the decryptor's error text reached the response: %q", internal.Message)
	}
	if logged := buf.String(); strings.Contains(logged, leaked) {
		t.Errorf("the decryptor's error text reached the log.\nlog: %s", logged)
	}
}

// TestListLinkedinAdsAdoptableCampaigns pins the candidate list: flag-gated, the account passed
// down, the project's own live rows marked, and an unaddressable account a 400.
func TestListLinkedinAdsAdoptableCampaigns(t *testing.T) {
	disp := &systemAccountAdopter{listRows: []model.AdoptableCampaign{
		{ID: "312", Name: "Hand built", Status: "ACTIVE"}, {ID: "313", Name: "Paused", Status: "PAUSED"},
	}}
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{}, scopeIDs: []string{"313"}}
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(camps, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: disp}))
	payload := &conn.ListLinkedinAdsAdoptableCampaignsPayload{ProjectID: "cncf", AccountID: "500928401"}

	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "")
	var bad *conn.BadRequestError
	if _, err := svc.ListLinkedinAdsAdoptableCampaigns(context.Background(), payload); !errors.As(err, &bad) || disp.listGot != "" {
		t.Fatalf("flag off = %v (dispatcher saw %q), want a 400 before the dispatcher", err, disp.listGot)
	}

	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	res, err := svc.ListLinkedinAdsAdoptableCampaigns(context.Background(), payload)
	if err != nil {
		t.Fatalf("ListLinkedinAdsAdoptableCampaigns: %v", err)
	}
	if disp.listGot != "500928401" || res.AccountID != "500928401" || res.AdoptableCount != 2 {
		t.Fatalf("result = %+v (dispatcher saw %q)", res, disp.listGot)
	}
	if res.Campaigns[0].RecordedInProject || !res.Campaigns[1].RecordedInProject || res.Campaigns[1].Status != "PAUSED" {
		t.Fatalf("campaigns = %+v %+v; want only 313 marked recorded", res.Campaigns[0], res.Campaigns[1])
	}

	disp.listErr = domain.ErrAccountNotManagedByConnection
	if _, err := svc.ListLinkedinAdsAdoptableCampaigns(context.Background(), payload); !errors.As(err, &bad) || !strings.Contains(bad.Message, "granted") {
		t.Fatalf("ungranted account = %v, want the 400", err)
	}
	// An untrustworthy candidate page (linkedin.ListAccountCampaignRefs' refusal, wrapped as the
	// dispatcher wraps it) is an upstream fault: 503, not a list.
	disp.listErr = fmt.Errorf("list linkedin adoptable campaigns: %w", linkedin.ErrUntrustworthyCampaignList)
	var unavailable *conn.ConnServiceUnavailableError
	if _, err := svc.ListLinkedinAdsAdoptableCampaigns(context.Background(), payload); !errors.As(err, &unavailable) {
		t.Fatalf("untrustworthy candidate page = %v, want the 503", err)
	}
	if _, err := svc.ListLinkedinAdsAdoptableCampaigns(context.Background(), &conn.ListLinkedinAdsAdoptableCampaignsPayload{ProjectID: model.SystemProjectID, AccountID: "500928401"}); err == nil {
		t.Fatal("the reserved LF scope was addressable")
	}
}

// TestAdoptCampaign_RepeatFindsTheAdoptedRowBehindANewerVersion: after campaign 312 is adopted and
// a new_version create puts 313 on the same slot, GetCampaignByPlatform answers 313 alone. An exact
// repeat of the 312 adoption must still find 312 (it is still bound to this brief) and answer it,
// without contacting LinkedIn, rather than the 409 for an occupied slot.
func TestAdoptCampaign_RepeatFindsTheAdoptedRowBehindANewerVersion(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	disp := &systemAccountAdopter{}
	s, camps := newAdoptService(t, model.ProviderLinkedInAds, disp)
	s.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	ctx := ctxWithActor(testActor)
	first, err := s.AdoptCampaign(ctx, systemAdoptPayload("500928401"))
	if err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	// What a new_version create leaves behind: a newer live version on the same slot.
	camps.mu.Lock()
	camps.storeRow(&model.Campaign{ID: "c-313", ProjectID: "cncf", BriefID: "b1",
		Platform: model.ProviderLinkedInAds, Variant: model.VariantDefault, SlotVersion: 2,
		PlatformCampaignID: "313", Status: "active"})
	camps.mu.Unlock()

	again, err := s.AdoptCampaign(ctx, systemAdoptPayload("500928401"))
	if err != nil {
		t.Fatalf("repeat adoption behind a newer version = %T %v, want the bound campaign", err, err)
	}
	if again.ID != first.ID || again.PlatformCampaignID == nil || *again.PlatformCampaignID != "312" {
		t.Fatalf("repeat answered %s, want the adopted 312 row %s", again.ID, first.ID)
	}
	if disp.calls != 1 || len(camps.adopted) != 1 {
		t.Fatalf("%d lookups, %d adopted rows; want 1 and 1 (the repeat must not contact LinkedIn)", disp.calls, len(camps.adopted))
	}
}

// TestAdoptCampaignHandler_DeniedAccountIDIs403 drives a denied account_id adoption through the
// generated HTTP handler and encoder: the service's ForbiddenError must reach the wire as 403,
// which needs adopt-campaign to declare and map Forbidden. Undeclared, it falls through to Goa's
// generic error encoder and the caller is told 500.
func TestAdoptCampaignHandler_DeniedAccountIDIs403(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	const briefID = "11111111-1111-4111-8111-111111111111"
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", briefID)] = &model.CampaignBrief{ID: briefID, ProjectID: "cncf", Status: model.BriefApproved}
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{}}
	jobs := newFakeJobRepo()
	disp := &systemAccountAdopter{}
	s := NewBriefService(repo, camps, jobs, NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: disp}))
	s.SetMarketingOpsChecker(&fakeMarketingOps{})

	auth := func(ctx context.Context, _ string, _ *security.JWTScheme) (context.Context, error) {
		return context.WithValue(ctx, actorCtxKey{}, testActor), nil
	}
	mux := goahttp.NewMuxer()
	briefssrv.MountAdoptCampaignHandler(mux, briefssrv.NewAdoptCampaignHandler(
		briefs.NewAdoptCampaignEndpoint(s, auth), mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, nil, nil))

	req := httptest.NewRequest(http.MethodPost, "/projects/cncf/briefs/"+briefID+"/campaigns/adopt",
		strings.NewReader(`{"platform":"linkedin-ads","platform_campaign_id":"312","account_id":"500928401"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "global_marketing_ops") {
		t.Fatalf("body %s does not name the role", rec.Body.String())
	}
	if disp.calls != 0 || len(camps.adopted) != 0 {
		t.Fatalf("a denied adoption reached LinkedIn %d time(s) / bound %d row(s)", disp.calls, len(camps.adopted))
	}
}

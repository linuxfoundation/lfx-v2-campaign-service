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

// adopterDispatcher implements ONLY CampaignAdopter, so a test that reaches Dispatch — i.e.
// creates upstream — fails loudly. calls pins how often the platform was contacted.
type adopterDispatcher struct {
	ref    *model.PlatformCampaignRef
	err    error
	calls  int
	gotID  string
	gotPrj string
}

func (adopterDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch must never be called on the adoption path: adoption binds an EXISTING campaign and must not create one upstream")
}

func (d *adopterDispatcher) LookupCampaign(_ context.Context, projectID string, _ model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	d.calls++
	d.gotPrj = projectID
	d.gotID = platformCampaignID
	return d.ref, d.err
}

// nonAdopterDispatcher implements PlatformDispatcher and nothing else.
type nonAdopterDispatcher struct{}

func (nonAdopterDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

// newAdoptService wires a BriefService whose brief is APPROVED and whose (brief, platform)
// pair has no campaign yet — the state adoption is for.
func newAdoptService(t *testing.T, platform model.Provider, disp PlatformDispatcher) (*BriefService, *fakeCampaignRepo) {
	t.Helper()
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefApproved,
	}
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{}}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{platform: disp})
	return NewBriefService(repo, camps, jobs, orch), camps
}

func adoptPayload() *briefs.AdoptCampaignPayload {
	return &briefs.AdoptCampaignPayload{
		ProjectID: "cncf", BriefID: "b1",
		Platform: string(model.ProviderGoogleAds), PlatformCampaignID: "1234567890",
	}
}

// The happy path binds what the PLATFORM reported, not what the caller asked for.
func TestAdoptCampaign_BindsThePlatformsOwnAnswer(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{
		ID: "1234567890", Name: "KubeCon EU 2026 — Search",
	}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

	res, err := s.AdoptCampaign(context.Background(), adoptPayload())
	if err != nil {
		t.Fatalf("AdoptCampaign: %v", err)
	}
	if disp.calls != 1 {
		t.Fatalf("platform was looked up %d times, want exactly 1", disp.calls)
	}
	if disp.gotID != "1234567890" || disp.gotPrj != "cncf" {
		t.Errorf("lookup got %q/%q, want cncf/1234567890", disp.gotPrj, disp.gotID)
	}
	if res.PlatformCampaignID == nil || *res.PlatformCampaignID != "1234567890" {
		t.Errorf("platform_campaign_id = %v, want 1234567890", res.PlatformCampaignID)
	}
	// The name comes from the platform read; the caller never sends one.
	if res.CampaignName != "KubeCon EU 2026 — Search" {
		t.Errorf("campaign_name = %q, want the platform's name", res.CampaignName)
	}
	// LIFECYCLE vocabulary, never the platform's run state: a row outside every status
	// predicate is undeletable AND never reconciled, because both default-deny.
	if res.Status != model.CampaignStatusCreated {
		t.Errorf("status = %q, want %q", res.Status, model.CampaignStatusCreated)
	}
	if !model.CampaignStatusDeletable(res.Status) {
		t.Errorf("the adopted row is not deletable; an operator could never unbind it")
	}
	if len(camps.adopted) != 1 {
		t.Fatalf("persisted %d campaigns via AdoptCampaign, want 1", len(camps.adopted))
	}
	if len(camps.upserted) != 0 {
		t.Errorf("adoption went through UpsertCampaign, which would overwrite an existing binding in place")
	}
	// Indexed like any other campaign write, or invisible to search until a later edit.
	if len(camps.indexPayloads) != 1 {
		t.Errorf("co-committed %d index messages, want 1", len(camps.indexPayloads))
	}
}

// A platform reporting a DIFFERENT id must not have the requested id written against it.
// A lookup that answers with a DIFFERENT campaign is not an adoption of that campaign. The
// earlier version of this test asserted the opposite — that the row carries whatever id the
// platform echoed — on the reasoning that recording the response faithfully beats recording
// the request. Both halves of that are wrong at once: binding campaign Y when the caller
// named campaign X hands a brief a real paid campaign nobody asked for, under a 201, and the
// only way the mismatch arises is an id filter that degraded to unfiltered, which means
// nothing else in the response is trustworthy either.
func TestAdoptCampaign_AMismatchedIDIsRefusedNotBound(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{
		ID: "9999999999", Name: "Someone else's campaign",
	}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	if err == nil {
		t.Fatal("a lookup that returned a different campaign was bound anyway")
	}
	// Unverifiable, never absent: nothing here shows the REQUESTED campaign is missing, and
	// a 404 is the answer an operator resolves by creating a duplicate.
	var nf *briefs.NotFoundError
	if errors.As(err, &nf) {
		t.Errorf("an unhonoured id filter was reported as a 404; that invites a duplicate paid campaign")
	}
	if len(camps.adopted) != 0 {
		t.Errorf("persisted %d campaign rows for a mismatched lookup; want 0", len(camps.adopted))
	}
}

// Absence — the answer an operator acts on by creating a duplicate — must persist nothing.
func TestAdoptCampaign_AbsentCampaignIs404AndPersistsNothing(t *testing.T) {
	disp := &adopterDispatcher{ref: nil, err: nil}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var nf *briefs.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("got %T (%v), want *briefs.NotFoundError", err, err)
	}
	if len(camps.adopted) != 0 {
		t.Errorf("persisted a campaign row for a campaign the platform says does not exist")
	}
}

// The counterpart: an answer we could not verify must NOT read as absence.
func TestAdoptCampaign_UnverifiableIsNeverReportedAsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport failure", errors.New("google-ads campaign lookup: Post \"https://googleads\": dial tcp: i/o timeout")},
		{"filter not honoured", errors.New("google-ads campaign lookup: query for campaign id 1234567890 returned campaign 42; the id filter was not honoured")},
		{"undecodable row", errors.New("google-ads campaign lookup: decoding result row: unexpected end of JSON input")},
		{"unrecognised status", errors.New("google-ads campaign lookup: campaign id 1234567890 has unrecognised status \"UNSPECIFIED\"")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &adopterDispatcher{err: tc.err}
			s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

			_, err := s.AdoptCampaign(context.Background(), adoptPayload())
			var nf *briefs.NotFoundError
			if errors.As(err, &nf) {
				t.Fatalf("an unverifiable lookup was reported as 404 \"no such campaign\"; an operator acting on that creates a DUPLICATE paid campaign alongside the one that is really there")
			}
			var unavailable *briefs.ConnServiceUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("got %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
			}
			if len(camps.adopted) != 0 {
				t.Errorf("persisted a binding for a campaign that was never verified")
			}
		})
	}
}

// An id-less ref, treated as success, writes a row every reader takes as provisioned.
func TestAdoptCampaign_RefWithNoIDIsNotSuccess(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "  ", Name: "n"}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	if err == nil {
		t.Fatal("a ref carrying no id was accepted as a successful adoption")
	}
	var nf *briefs.NotFoundError
	if errors.As(err, &nf) {
		t.Error("an id-less ref was reported as absence; the platform DID answer, it answered unusably")
	}
	if len(camps.adopted) != 0 {
		t.Errorf("persisted a campaign row with no upstream id")
	}
}

// Re-adopting a bound pair must not repoint it and orphan a still-spending campaign.
func TestAdoptCampaign_AlreadyBoundPairIsAConflict(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "1234567890", Name: "n"}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)
	camps.existing = map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {
			ID: "c-old", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
			PlatformCampaignID: "1111111111", Status: model.CampaignStatusCreated,
		},
	}

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("got %T (%v), want *briefs.ConflictError", err, err)
	}
	if got := camps.existing["b1|"+string(model.ProviderGoogleAds)].PlatformCampaignID; got != "1111111111" {
		t.Errorf("the existing binding was repointed to %q; the campaign it used to name is now orphaned upstream and still spending", got)
	}
}

// No adoption capability is a 400, not a 503 inviting a retry of something time cannot fix.
func TestAdoptCampaign_UnsupportedPlatformIs400(t *testing.T) {
	s, camps := newAdoptService(t, model.ProviderGoogleAds, nonAdopterDispatcher{})

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("got %T (%v), want *briefs.BadRequestError", err, err)
	}
	if len(camps.adopted) != 0 {
		t.Errorf("persisted a binding for a platform that cannot verify it")
	}
}

// Each of these sentinels is WRAPPED ALONGSIDE the more general one below it, so the arms are
// ordered narrowest-first and a broad match placed first silently swallows the narrow case.
// The system row is the sharp one: it is a 500 for an operator, not a 409 telling a project
// with no connection of its own to go repair one. Asserting the TYPE, not just the message, is
// what makes a deleted arm fail here instead of falling through to a plausible neighbour.
func TestAdoptCampaign_ConnectionDefectsAreDistinguished(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantInMsg string
		want500   bool
	}{
		{
			name:    "the shared LF system connection, which the caller cannot repair",
			err:     fmt.Errorf("%w: %w: %w", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete),
			want500: true,
		},
		{
			name:      "no account selected",
			err:       fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected),
			wantInMsg: "no ad account selected",
		},
		{
			name:      "credentials unusable",
			err:       fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete),
			wantInMsg: "repair the connection",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &adopterDispatcher{err: tc.err}
			s, _ := newAdoptService(t, model.ProviderGoogleAds, disp)

			_, err := s.AdoptCampaign(context.Background(), adoptPayload())
			if tc.want500 {
				var ise *briefs.InternalServerError
				if !errors.As(err, &ise) {
					t.Fatalf("got %T (%v), want *briefs.InternalServerError — a defect in the LF system "+
						"row is an operator page, not a repair instruction for this project", err, err)
				}
				return
			}
			var conflict *briefs.ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("got %T (%v), want *briefs.ConflictError", err, err)
			}
			if !strings.Contains(conflict.Message, tc.wantInMsg) {
				t.Errorf("409 message %q does not name what to fix (want it to mention %q)", conflict.Message, tc.wantInMsg)
			}
		})
	}
}

// A bad request must never be an oracle for which campaign ids exist on a hidden account.
func TestAdoptCampaign_RejectionsNeverReachThePlatform(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*briefs.AdoptCampaignPayload)
		approve bool
	}{
		{"unknown platform", func(p *briefs.AdoptCampaignPayload) { p.Platform = "myspace_ads" }, true},
		{"whitespace-only id", func(p *briefs.AdoptCampaignPayload) { p.PlatformCampaignID = "   " }, true},
		{"non-slug project scope", func(p *briefs.AdoptCampaignPayload) { p.ProjectID = "NOT A SLUG" }, true},
		{"unapproved brief", func(*briefs.AdoptCampaignPayload) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "1234567890"}}
			s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)
			if !tc.approve {
				s.briefs.(*fakeBriefRepo).briefs[briefKey("cncf", "b1")].Status = model.BriefDraft
			}
			p := adoptPayload()
			tc.mutate(p)

			_, err := s.AdoptCampaign(context.Background(), p)
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("got %T (%v), want *briefs.BadRequestError", err, err)
			}
			if disp.calls != 0 {
				t.Errorf("the ad platform was contacted %d times for a request rejectable without it", disp.calls)
			}
			if len(camps.adopted) != 0 {
				t.Errorf("persisted a binding for a rejected request")
			}
		})
	}
}

// A brief that does not exist, or belongs elsewhere, is a 404 decided locally.
func TestAdoptCampaign_UnknownBriefIs404BeforeThePlatform(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "1234567890"}}
	s, _ := newAdoptService(t, model.ProviderGoogleAds, disp)

	p := adoptPayload()
	p.BriefID = "does-not-exist"
	_, err := s.AdoptCampaign(context.Background(), p)
	var nf *briefs.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("got %T (%v), want *briefs.NotFoundError", err, err)
	}
	if disp.calls != 0 {
		t.Errorf("the ad platform was contacted for a brief this project does not have — adoption must not double as an id oracle")
	}
}

// Absence becomes a sentinel rather than a nil ref, so no caller can dereference nil on a
// nil error.
func TestLookupPlatformCampaign_AbsenceIsASentinelNotANilRef(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: &adopterDispatcher{}})

	ref, err := orch.LookupPlatformCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
	if ref != nil {
		t.Errorf("got a non-nil ref alongside the absence sentinel")
	}
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("got %v, want ErrPlatformCampaignAbsent", err)
	}
}

func TestLookupPlatformCampaign_NoAdopterAndNoDispatcherBothReportUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dispatchers map[model.Provider]PlatformDispatcher
	}{
		{"dispatcher is not an adopter", map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: nonAdopterDispatcher{}}},
		{"no dispatcher at all", map[model.Provider]PlatformDispatcher{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), tc.dispatchers)
			_, err := orch.LookupPlatformCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
			if !errors.Is(err, domain.ErrAdoptionUnsupported) {
				t.Fatalf("got %v, want ErrAdoptionUnsupported", err)
			}
		})
	}
}

// An empty id on a client whose filter degrades to "unfiltered" adopts someone else's.
func TestLookupPlatformCampaign_EmptyIDNeverReachesThePlatform(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "1234567890"}}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	if _, err := orch.LookupPlatformCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "  "); err == nil {
		t.Fatal("an empty platform campaign id was accepted")
	}
	if disp.calls != 0 {
		t.Errorf("the platform was contacted %d times with an empty id", disp.calls)
	}
}

// The lookup is bounded, like every other synchronous platform call on a request path.
func TestLookupPlatformCampaign_IsBounded(t *testing.T) {
	var hadDeadline bool
	disp := &deadlineRecordingAdopter{seen: &hadDeadline}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	_, _ = orch.LookupPlatformCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
	if !hadDeadline {
		t.Error("LookupCampaign ran on a context with no deadline; a hung platform call would hold the request open indefinitely")
	}
}

type deadlineRecordingAdopter struct{ seen *bool }

func (deadlineRecordingAdopter) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d deadlineRecordingAdopter) LookupCampaign(ctx context.Context, _ string, _ model.Provider, _ string) (*model.PlatformCampaignRef, error) {
	_, ok := ctx.Deadline()
	*d.seen = ok
	return &model.PlatformCampaignRef{ID: "1234567890"}, nil
}

// The adopted row must record its provenance exactly as a created row does — the account it
// was verified under, and who bound it. Without the account, googleAdsCreationCustomerID reads
// "unknown", the account-mismatch guards treat that as permission to proceed, and once the
// project's connection is repointed the same numeric id addresses a different customer's
// campaign. Without the actor, an adopted campaign has no audit trail at all.
func TestAdoptCampaign_RecordsItsProvenance(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{
		ID: "1234567890", Name: "KubeCon EU 2026 — Search",
		Result: json.RawMessage(`{"customerId":"1112223333"}`),
	}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)
	ctx := ctxWithActor(&model.Actor{Username: "mrautela"})

	if _, err := s.AdoptCampaign(ctx, adoptPayload()); err != nil {
		t.Fatalf("AdoptCampaign: %v", err)
	}
	var got struct {
		CustomerID string `json:"customerId"`
	}
	if err := json.Unmarshal(camps.adopted[0].Result, &got); err != nil {
		t.Fatalf("the adopted row has no readable provenance blob (%q): %v", camps.adopted[0].Result, err)
	}
	if got.CustomerID != "1112223333" {
		t.Errorf("persisted customerId = %q, want 1112223333 — the account-mismatch guards read this back", got.CustomerID)
	}
	if by := camps.adopted[0].CreatedBy; by == nil || by.Username != "mrautela" {
		t.Errorf("created_by = %v, want the authenticated actor", by)
	}
}

// Approval is read BEFORE a platform lookup bounded at 20 seconds, so a concurrent replace or
// archive can land inside that window and the insert would bind paid spend to a brief that is no
// longer approved — the approval gate routed around by latency alone. The version the service
// verified must therefore reach the repository, which re-checks it under the row lock.
func TestAdoptCampaign_ApprovalLostDuringTheLookupIsAConflict(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "1234567890", Name: "n"}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)
	// What the locked re-read finds: the brief moved on while the platform was being queried.
	camps.adoptBriefVersion = 7

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("got %T (%v), want *briefs.ConflictError — a brief that lost approval mid-lookup must not be bound", err, err)
	}
	if len(camps.adopted) != 0 {
		t.Errorf("bound a campaign to a brief that is no longer approved at the verified version")
	}
}

// One upstream campaign, one live binding. Two rows pointing at the same paid campaign each
// think they own it: one brief's toggle pauses what the other just enabled, and no reader of
// either row can see why. Neither row is malformed, so nothing detects it after the fact.
func TestAdoptCampaign_SecondBriefCannotBindTheSameCampaign(t *testing.T) {
	disp := &adopterDispatcher{ref: &model.PlatformCampaignRef{ID: "1234567890", Name: "n"}}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)
	camps.existing = map[string]*model.Campaign{
		"b0|" + string(model.ProviderGoogleAds): {
			ID: "c-first", ProjectID: "cncf", BriefID: "b0", Platform: model.ProviderGoogleAds,
			PlatformCampaignID: "1234567890", Status: model.CampaignStatusCreated,
		},
	}

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("got %T (%v), want *briefs.ConflictError", err, err)
	}
	// The message must name the OTHER binding. "this brief already has a campaign" sends the
	// operator to inspect a brief that has none, and the real second binding goes unnoticed.
	if !strings.Contains(conflict.Message, "another brief") {
		t.Errorf("409 message %q does not say the campaign is bound elsewhere", conflict.Message)
	}
	if len(camps.adopted) != 0 {
		t.Errorf("created a second live binding for one upstream campaign")
	}
}

// A malformed id is rejected by the adapter locally, before any query. Reporting that as 503
// tells the caller to retry input that can only ever fail; it is a 400.
func TestAdoptCampaign_MalformedPlatformIDIs400(t *testing.T) {
	disp := &adopterDispatcher{err: fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, errors.New(`"007" is not a campaign id`))}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("got %T (%v), want *briefs.BadRequestError — a permanently invalid id is not an unreachable platform", err, err)
	}
	if len(camps.adopted) != 0 {
		t.Errorf("persisted %d campaigns for a malformed id, want 0", len(camps.adopted))
	}
}

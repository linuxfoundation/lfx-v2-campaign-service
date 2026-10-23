// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goahttp "goa.design/goa/v3/http"

	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// planService builds the handler over a library of stored rows, oldest first exactly as the
// repository returns them.
func planService(assets ...*model.CreativeAsset) *BriefService {
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(&fakeCreativeAssetRepo{list: assets})
	return s
}

func planAsset(id string, w, h int) *model.CreativeAsset {
	return &model.CreativeAsset{
		ID: id, ProjectID: "p1", BriefID: "b1",
		MimeType: model.MimeTypePNG, ByteSize: 1024, Checksum: "c-" + id,
		Width: w, Height: h,
	}
}

func getPlan(t *testing.T, s *BriefService, platforms ...string) *briefs.CreativeAssetPlan {
	t.Helper()
	res, err := s.GetAssetPlan(context.Background(), &briefs.GetAssetPlanPayload{
		ProjectID: "p1", BriefID: "b1", Platforms: platforms,
	})
	if err != nil {
		t.Fatalf("GetAssetPlan: %v", err)
	}
	return res
}

// A library with every Google shape in it must leave NO gaps. This is the endpoint's headline
// answer — "this brief can launch" — and it is the one a client renders as a green state, so it
// has to be reachable with assets an operator could actually produce.
func TestGetAssetPlan_CompleteLibraryHasNoGaps(t *testing.T) {
	s := planService(
		planAsset("landscape", 1910, 1000), // 1.91:1
		planAsset("square", 1200, 1200),    // 1:1
		planAsset("portrait", 960, 1200),   // 4:5
		planAsset("tall", 900, 1600),       // 9:16
		planAsset("logo-sq", 1200, 1200),
		planAsset("logo-wide", 1200, 300), // 4:1
	)

	plan := getPlan(t, s)

	if len(plan.Gaps) != 0 {
		for _, g := range plan.Gaps {
			t.Errorf("unexpected gap: %s — %s", g.Label, g.Reason)
		}
		t.Fatalf("a library with every documented shape must produce no gaps")
	}
	if len(plan.Assignments) == 0 {
		t.Fatal("no slot was filled at all")
	}
	if len(plan.Unclassified) != 0 {
		t.Errorf("every asset has dimensions, so none may be unclassified: %v", plan.Unclassified)
	}
}

// An EMPTY library is the state every brief starts in, and it is where the plan earns its keep:
// the response must be a full list of what to go and make, not an error and not a blank.
func TestGetAssetPlan_EmptyLibraryReportsEveryRequirement(t *testing.T) {
	plan := getPlan(t, planService())

	if len(plan.Assignments) != 0 {
		t.Errorf("nothing is stored, so nothing can be assigned: %+v", plan.Assignments)
	}
	if len(plan.Gaps) == 0 {
		t.Fatal("an empty library must report the platform's required slots as gaps")
	}
	for _, g := range plan.Gaps {
		if g.Need < 1 {
			t.Errorf("gap %q has need = %d; a gap exists only when something is still needed", g.Label, g.Need)
		}
		// The reason is the whole value of a gap: it is what the operator hands a designer.
		// "No eligible asset" would be a true sentence and a useless one.
		if !strings.Contains(g.Reason, "at least") {
			t.Errorf("gap %q must state the pixel floor a designer needs: %q", g.Label, g.Reason)
		}
		if g.Platform != "google-ads" {
			t.Errorf("gap %q carries platform %q, want google-ads", g.Label, g.Platform)
		}
	}
}

// Every list is REQUIRED on the wire, and an empty one is a real answer — "no gaps" is the
// answer that means ready to launch. A nil slice marshals as null, which a client iterating the
// list has to special-case or crash on, so the handler must hand the transport [] instead.
func TestGetAssetPlan_EmptyListsAreArraysNotNull(t *testing.T) {
	// A library that fills everything: gaps, unclassified and unused are all empty here, which
	// is precisely the combination that would otherwise serialize as three nulls.
	plan := getPlan(t, planService(
		planAsset("landscape", 1910, 1000), planAsset("square", 1200, 1200), planAsset("portrait", 960, 1200),
		planAsset("tall", 900, 1600), planAsset("logo-wide", 1200, 300),
	))

	if plan.Gaps == nil || plan.Unclassified == nil || plan.Unused == nil || plan.Assignments == nil {
		t.Fatalf("nil slice in the result: assignments=%v gaps=%v unclassified=%v unused=%v",
			plan.Assignments == nil, plan.Gaps == nil, plan.Unclassified == nil, plan.Unused == nil)
	}
	for _, a := range plan.Assignments {
		if a.AssetIds == nil {
			t.Errorf("slot %s has a nil asset_ids", a.SlotKey)
		}
	}
}

// An asset stored before migration 000047 has no dimensions. It must read as UNCLASSIFIED, never
// as an asset that failed a shape: unknown and wrong are different answers, and only one of them
// is fixed by a designer. Telling an operator their whole pre-existing library is the wrong size
// would send them to redo work a dimension backfill does for free.
func TestGetAssetPlan_UndimensionedAssetsAreUnclassified(t *testing.T) {
	old := &model.CreativeAsset{
		ID: "legacy", ProjectID: "p1", BriefID: "b1",
		MimeType: model.MimeTypePNG, ByteSize: 2048, Checksum: "c-legacy",
	}
	s := planService(old, planAsset("landscape", 1910, 1000))

	plan := getPlan(t, s)

	if len(plan.Unclassified) != 1 || plan.Unclassified[0] != "legacy" {
		t.Fatalf("unclassified = %v, want [legacy]", plan.Unclassified)
	}
	for _, id := range plan.Unused {
		if id == "legacy" {
			t.Error("an unclassified asset must not also be reported as unused")
		}
	}
	for _, g := range plan.Gaps {
		if strings.Contains(g.Reason, "legacy") {
			t.Errorf("no gap may blame the undimensioned asset: %q", g.Reason)
		}
	}
}

// The mirror of a gap: a file the operator just dropped that no requested slot will take. Without
// it the plan answers only half the question after a folder upload.
func TestGetAssetPlan_UnusableAssetIsReportedUnused(t *testing.T) {
	// 50x50 is a real image with real dimensions and below every Google floor.
	plan := getPlan(t, planService(planAsset("tiny", 50, 50)))

	if len(plan.Unused) != 1 || plan.Unused[0] != "tiny" {
		t.Fatalf("unused = %v, want [tiny]", plan.Unused)
	}
	if len(plan.Unclassified) != 0 {
		t.Errorf("the asset's dimensions ARE known, so it is unusable rather than unclassified: %v", plan.Unclassified)
	}
}

// Ordinal comes from the repository's oldest-first list order, and it is the classifier's last
// tie-break. Two identical images must therefore resolve the same way on every call — an
// operator who refreshes the page must not see the assignment reshuffle under them.
func TestGetAssetPlan_IsStableAcrossCalls(t *testing.T) {
	s := planService(
		planAsset("first", 1910, 1000),
		planAsset("second", 1910, 1000),
	)

	want := getPlan(t, s)
	for i := 0; i < 3; i++ {
		got := getPlan(t, s)
		if len(got.Assignments) != len(want.Assignments) {
			t.Fatalf("call %d returned %d assignments, first call returned %d", i, len(got.Assignments), len(want.Assignments))
		}
		for j := range got.Assignments {
			if got.Assignments[j].SlotKey != want.Assignments[j].SlotKey ||
				strings.Join(got.Assignments[j].AssetIds, ",") != strings.Join(want.Assignments[j].AssetIds, ",") {
				t.Fatalf("call %d slot %s got %v, first call got %v",
					i, got.Assignments[j].SlotKey, got.Assignments[j].AssetIds, want.Assignments[j].AssetIds)
			}
		}
	}
	// The earlier upload wins the tie, which is what "stable" is anchored to.
	for _, a := range want.Assignments {
		if len(a.AssetIds) > 0 && a.AssetIds[0] != "first" {
			t.Errorf("slot %s placed %q first; the earlier upload must win an exact tie", a.SlotKey, a.AssetIds[0])
		}
	}
}

// The filter's three behaviours, each of which a client depends on.
func TestGetAssetPlan_PlatformsFilter(t *testing.T) {
	s := planService(planAsset("landscape", 1910, 1000))

	t.Run("absent means every platform the service publishes", func(t *testing.T) {
		plan := getPlan(t, s)
		if len(plan.Platforms) == 0 {
			t.Fatal("platforms is empty; the default must name what was actually evaluated")
		}
		// Bound to the registry rather than to a literal: a platform added there must show up
		// here without this test being edited, which is the point of the default.
		if len(plan.Platforms) != len(slotCatalogs()) {
			t.Errorf("platforms = %v, want every key of the registry (%d)", plan.Platforms, len(slotCatalogs()))
		}
	})

	t.Run("a known platform is echoed back", func(t *testing.T) {
		plan := getPlan(t, s, "google-ads")
		if len(plan.Platforms) != 1 || plan.Platforms[0] != "google-ads" {
			t.Fatalf("platforms = %v, want [google-ads]", plan.Platforms)
		}
		if len(plan.Assignments) == 0 {
			t.Error("the landscape image fits a Google slot, so the filter must not have excluded it")
		}
	})

	// An unknown platform is dropped, not refused. It publishes no slots, so it can produce
	// neither an assignment nor a gap — and a client built against a newer release must get the
	// plan for the platforms this one does know rather than a 400 and nothing. Dropping it is
	// visible in the echo, so the caller can still tell what it asked for is not what it got.
	t.Run("an unknown platform is ignored, not an error", func(t *testing.T) {
		plan := getPlan(t, s, "tiktok-ads")
		if len(plan.Platforms) != 0 {
			t.Errorf("platforms = %v, want empty — the request named no platform this service knows", plan.Platforms)
		}
		if len(plan.Assignments) != 0 || len(plan.Gaps) != 0 {
			t.Errorf("a catalog with no slots can produce neither assignments nor gaps: %d/%d",
				len(plan.Assignments), len(plan.Gaps))
		}
		if len(plan.Unused) != 1 {
			t.Errorf("unused = %v, want the one asset no requested slot accepts", plan.Unused)
		}
	})

	t.Run("a duplicated platform is evaluated once", func(t *testing.T) {
		plan := getPlan(t, s, "google-ads", "google-ads")
		if len(plan.Platforms) != 1 {
			t.Fatalf("platforms = %v, want [google-ads] — a repeat must not double the catalog", plan.Platforms)
		}
		single := getPlan(t, s, "google-ads")
		if len(plan.Assignments) != len(single.Assignments) || len(plan.Gaps) != len(single.Gaps) {
			t.Errorf("duplicated filter changed the plan: %d/%d vs %d/%d",
				len(plan.Assignments), len(plan.Gaps), len(single.Assignments), len(single.Gaps))
		}
	})
}

// Google requires Demand Gen's landscape and square marketing images only of EACH OTHER. The
// endpoint has to carry that faithfully in both directions: one of them satisfies it, and neither
// produces a single gap that offers both shapes and claims no individual slot.
func TestGetAssetPlan_ReciprocalRequirementSurvivesToTheWire(t *testing.T) {
	const label = "Demand Gen marketing image, landscape or square"

	t.Run("the square alone satisfies it", func(t *testing.T) {
		plan := getPlan(t, planService(planAsset("square", 1200, 1200)), "google-ads")
		for _, g := range plan.Gaps {
			if g.Label == label {
				t.Fatalf("the square is present, so the either-or rule is met: %q", g.Reason)
			}
		}
	})

	t.Run("neither leaves one gap naming both", func(t *testing.T) {
		plan := getPlan(t, planService(), "google-ads")
		var found *briefs.CreativeSlotGap
		for _, g := range plan.Gaps {
			if g.Label == label {
				found = g
			}
		}
		if found == nil {
			t.Fatal("an empty library must report the reciprocal requirement")
		}
		// Absent slot_key is the wire-level expression of "this belongs to the pair". Naming one
		// member would send the operator to produce exactly the image they did not need.
		if found.SlotKey != nil {
			t.Errorf("slot_key = %q, want absent for a requirement spanning several slots", *found.SlotKey)
		}
		if !strings.Contains(found.Reason, "191:100") || !strings.Contains(found.Reason, "1:1") {
			t.Errorf("the reason must offer BOTH shapes: %q", found.Reason)
		}
	})
}

// Channel is OPTIONAL on the wire — a platform with a single ad shape has none — but every Google
// slot has one, and a plan row without it could not say which of three "logo image" slots it is.
func TestGetAssetPlan_RowsCarryTheirChannelAndRequirement(t *testing.T) {
	plan := getPlan(t, planService(planAsset("square", 1200, 1200)), "google-ads")

	if len(plan.Assignments) == 0 {
		t.Fatal("the square fits several Google slots")
	}
	for _, a := range plan.Assignments {
		if a.Channel == nil || *a.Channel == "" {
			t.Errorf("slot %s has no channel; every Google slot belongs to one", a.SlotKey)
		}
		if a.Requirement == "" {
			t.Errorf("slot %s published no requirement sentence", a.SlotKey)
		}
		// slot_key is the join key an override is sent back under, so it must be the full
		// platform/channel/slot triple and not just the wire name.
		if !strings.HasPrefix(a.SlotKey, "google-ads/") {
			t.Errorf("slot_key = %q, want the platform-qualified key", a.SlotKey)
		}
	}
}

// The published min/max counts must be the catalog's, because a client renders "1 of 2 required"
// from them. A literal copied into the mapper would compile and quietly disagree with the rules
// the gaps were computed from.
func TestGetAssetPlan_CountsComeFromTheCatalog(t *testing.T) {
	plan := getPlan(t, planService(
		planAsset("landscape", 1910, 1000), planAsset("square", 1200, 1200), planAsset("logo-wide", 1200, 300),
	), "google-ads")

	byKey := map[string]int{}
	for _, slot := range googleads.ImageSlotCatalog().Slots {
		byKey[slot.Key()] = slot.MinCount
	}
	if len(byKey) == 0 {
		t.Fatal("the Google catalog published no slots")
	}
	checked := 0
	for _, a := range plan.Assignments {
		want, ok := byKey[a.SlotKey]
		if !ok {
			t.Fatalf("plan published slot %q, which the catalog does not contain", a.SlotKey)
		}
		if a.MinCount != want {
			t.Errorf("slot %s min_count = %d, want the catalog's %d", a.SlotKey, a.MinCount, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no assignment was checked")
	}
}

// Without the repository the handler reaches a nil interface. That is a normal deployment — the
// creative-asset feature is config-gated — so it must be a 503, not a panic that takes the
// process down with one GET.
func TestGetAssetPlan_UnavailableWithoutRepo(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	_, err := s.GetAssetPlan(context.Background(), &briefs.GetAssetPlanPayload{ProjectID: "p1", BriefID: "b1"})
	assertCreativeAssetUnavailable(t, err, "creative asset reads are unavailable")
}

// A repository failure maps through the same translator every other brief read uses, so an
// archived brief is a 404 here exactly as it is on the list endpoint.
func TestGetAssetPlan_MapsRepositoryErrors(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(&fakeCreativeAssetRepo{listErr: domain.ErrNotFound})

	_, err := s.GetAssetPlan(context.Background(), &briefs.GetAssetPlanPayload{ProjectID: "p1", BriefID: "b1"})
	var notFound *briefs.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %T (%v), want *briefs.NotFoundError", err, err)
	}
	if notFound.Code != "404" {
		t.Errorf("code = %q, want 404", notFound.Code)
	}
}

// The plan route is a LITERAL segment under the same prefix as get-creative-asset's {asset_id}
// wildcard. If the muxer preferred the wildcard, every call to the plan endpoint would arrive at
// the single-asset handler as asset_id="plan" — a 404 that looks like a missing asset and says
// nothing about the real cause. This pins the resolution at the generated mux, which is the only
// layer that decides it.
func TestGetAssetPlanRoute_WinsAgainstTheAssetIDWildcard(t *testing.T) {
	mux := goahttp.NewMuxer()
	got := ""
	mux.Handle(http.MethodGet, "/projects/{project_id}/briefs/{brief_id}/creative-assets/{asset_id}",
		func(http.ResponseWriter, *http.Request) { got = "asset" })
	mux.Handle(http.MethodGet, "/projects/{project_id}/briefs/{brief_id}/creative-assets/plan",
		func(http.ResponseWriter, *http.Request) { got = "plan" })

	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/projects/p1/briefs/b1/creative-assets/plan", nil))
	if got != "plan" {
		t.Fatalf("routed to %q, want the plan handler — the literal segment must beat {asset_id}", got)
	}

	got = ""
	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/projects/p1/briefs/b1/creative-assets/a-real-id", nil))
	if got != "asset" {
		t.Fatalf("routed to %q, want the single-asset handler", got)
	}
}

// The repeatable query parameter must decode to the slice the handler filters on. Pinning it at
// the generated decoder is what makes a design change to `platforms` a loud failure rather than a
// filter that silently stops filtering.
func TestGetAssetPlanDecoder_ReadsRepeatedPlatforms(t *testing.T) {
	// briefIDAttr() carries Format(FormatUUID), so the generated decoder validates the path
	// segment before the handler ever sees it — a short stub id fails validation, not routing.
	// project_id has no format, which is why only one of the two has to be a real UUID here.
	const planBriefUUID = "6f1a5a6e-4f24-4d9f-9d2f-0a1b2c3d4e5f"

	mux := goahttp.NewMuxer()
	decode := briefsserver.DecodeGetAssetPlanRequest(mux, goahttp.RequestDecoder)

	var routed *http.Request
	mux.Handle(http.MethodGet, "/projects/{project_id}/briefs/{brief_id}/creative-assets/plan",
		func(_ http.ResponseWriter, r *http.Request) { routed = r })

	newReq := func(t *testing.T, query string) *http.Request {
		t.Helper()
		routed = nil
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
			"/projects/p1/briefs/"+planBriefUUID+"/creative-assets/plan"+query, nil))
		if routed == nil {
			t.Fatal("request was not routed; the decoder would not see path params")
		}
		return routed
	}

	payload, err := decode(newReq(t, "?platforms=google-ads&platforms=meta-ads"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Platforms) != 2 || payload.Platforms[0] != "google-ads" || payload.Platforms[1] != "meta-ads" {
		t.Errorf("platforms = %v, want both values in order", payload.Platforms)
	}
	if payload.ProjectID != "p1" || payload.BriefID != planBriefUUID {
		t.Errorf("ids = %q/%q, want p1/%s", payload.ProjectID, payload.BriefID, planBriefUUID)
	}

	// Omitting the parameter must leave the slice EMPTY rather than producing a validation
	// error: absent is the default-to-everything case, not a malformed request.
	bare, err := decode(newReq(t, ""))
	if err != nil {
		t.Fatalf("a request with no platforms filter must decode: %v", err)
	}
	if len(bare.Platforms) != 0 {
		t.Errorf("platforms = %v, want empty when the parameter is absent", bare.Platforms)
	}
}

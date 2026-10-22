// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sort"
	"strings"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/creative"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// slotCatalogs is the registry: every platform whose image rules this service publishes, keyed
// by the platform identifier a campaign config carries.
//
// It is assembled HERE rather than inside internal/creative, and that direction is deliberate.
// The classifier depends on nothing, each platform package publishes its own rows
// (googleads.ImageSlotCatalog), and this layer — which already imports every platform — is the
// one place that can name them all without creating an import cycle or dragging every platform
// client into a binary that only wanted to classify one image.
//
// Adding a platform is one entry here plus a Catalog in that platform's package. Nothing in
// internal/creative changes.
func slotCatalogs() map[string]creative.Catalog {
	return map[string]creative.Catalog{
		"google-ads": googleads.ImageSlotCatalog(),
	}
}

// GetAssetPlan matches a brief's stored assets against the image slots the requested platforms
// publish.
//
// It is a PURE READ, and that is a contract rather than an implementation detail: it stores
// nothing, attaches nothing to a campaign, and returns an identical plan for an unchanged
// library. An operator may therefore call it as often as they like while still deciding, and
// the override they eventually send replaces a row in this plan rather than racing it.
func (s *BriefService) GetAssetPlan(ctx context.Context, p *briefs.GetAssetPlanPayload) (*briefs.CreativeAssetPlan, error) {
	repo := s.creativeAssetRepo()
	if repo == nil {
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "creative asset reads are unavailable"}
	}

	// ListAssets, not a bytes-bearing read: classifying needs an id and a shape, both of which
	// are columns on the row. Loading up to 30 MiB of BYTEA per asset to compare two integers
	// would be the whole cost of the endpoint and buy nothing.
	// The two discarded returns are the brief's count and stored bytes. They are deliberately
	// not echoed here: list-creative-assets is where a client reads its quota headroom, and
	// publishing the same two figures from a second endpoint gives a UI two sources for one
	// number that can disagree by a request. This endpoint answers a different question.
	assets, _, _, err := repo.ListAssets(ctx, p.ProjectID, p.BriefID)
	if err != nil {
		return nil, mapBriefErr(err)
	}

	platforms, catalog := requestedCatalog(p.Platforms)
	plan := creative.Classify(catalog, classifierAssets(assets))

	return &briefs.CreativeAssetPlan{
		Platforms: platforms,
		// Every list is a non-nil slice. An empty `gaps` is the answer that means "this library
		// can launch", so a client must be able to tell it from a field the server declined to
		// compute — and a nil slice renders as JSON null, which is that other answer.
		Assignments:  assignmentResults(plan.Assignments),
		Gaps:         gapResults(plan.Gaps),
		Unclassified: idList(plan.Unclassified),
		Unused:       idList(plan.Unused),
	}, nil
}

// requestedCatalog resolves the platforms filter to the catalog that will be evaluated, and
// returns the platform list alongside it so the response can echo what was actually covered.
//
// An EMPTY filter means every platform the registry knows, which is the answer an operator
// asking "is my library ready" wants. An UNKNOWN platform is dropped rather than refused: it
// publishes no slots, so it can produce neither an assignment nor a gap, and a client built
// against a newer release should get the plan for the platforms this one does know rather than
// a 400 and nothing. Dropping it is visible in the echoed `platforms`, so the caller can still
// tell that what it asked for is not what it got.
//
// The result is sorted so a caller that sends the same set in a different order gets the same
// plan — the classifier fills slots in catalog order, and an unsorted map range would otherwise
// make a shared group ceiling fall to a different platform between two identical requests.
func requestedCatalog(requested []string) ([]string, creative.Catalog) {
	registry := slotCatalogs()

	names := make([]string, 0, len(registry))
	if len(requested) == 0 {
		for name := range registry {
			names = append(names, name)
		}
	} else {
		seen := make(map[string]bool, len(requested))
		for _, r := range requested {
			name := strings.TrimSpace(r)
			if _, ok := registry[name]; !ok || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)

	parts := make([]creative.Catalog, 0, len(names))
	for _, name := range names {
		parts = append(parts, registry[name])
	}
	return names, creative.Merge(parts...)
}

// classifierAssets maps stored rows onto the classifier's view of an asset.
//
// Ordinal comes from the LIST INDEX, which is the repository's oldest-first order. It is the
// classifier's last ranking tie-break, so it is what makes a plan stable across reloads — and it
// is an index rather than a created_at because two assets uploaded in one batch can share a
// timestamp, and an index cannot.
//
// Width and Height are copied as they are, including the zero pair an asset stored before
// migration 000047 carries. Zero means UNKNOWN to the classifier, which reports those assets as
// unclassified instead of judging them against a shape they never declared.
func classifierAssets(assets []*model.CreativeAsset) []creative.Asset {
	out := make([]creative.Asset, 0, len(assets))
	for i, a := range assets {
		out = append(out, creative.Asset{
			ID:       a.ID,
			Width:    a.Width,
			Height:   a.Height,
			ByteSize: a.ByteSize,
			Ordinal:  i,
		})
	}
	return out
}

func assignmentResults(in []creative.Assignment) []*briefs.CreativeSlotAssignment {
	out := make([]*briefs.CreativeSlotAssignment, 0, len(in))
	for _, a := range in {
		row := &briefs.CreativeSlotAssignment{
			SlotKey:  a.Slot.Key(),
			Platform: a.Slot.Platform,
			Slot:     a.Slot.Slot,
			Label:    a.Slot.Label,
			// The slot's rule travels with a FULL slot too: the operator's next question after
			// "what fits here" is "what else would", and re-deriving the sentence client-side
			// would mean keeping a second copy of every platform's geometry in the browser.
			Requirement: a.Slot.Describe(),
			AssetIds:    idList(a.AssetIDs),
			MinCount:    a.Slot.MinCount,
			MaxCount:    a.Slot.MaxCount,
		}
		if a.Slot.Channel != "" {
			ch := a.Slot.Channel
			row.Channel = &ch
		}
		out = append(out, row)
	}
	return out
}

func gapResults(in []creative.Gap) []*briefs.CreativeSlotGap {
	out := make([]*briefs.CreativeSlotGap, 0, len(in))
	for _, g := range in {
		row := &briefs.CreativeSlotGap{
			Platform: g.Platform,
			Label:    g.Label,
			Need:     g.Need,
			Reason:   g.Reason,
		}
		if g.Channel != "" {
			ch := g.Channel
			row.Channel = &ch
		}
		// SlotKey stays ABSENT for a requirement spanning several slots — Google's reciprocal
		// landscape-or-square pair belongs to neither member alone, and naming one of them would
		// send the operator to make exactly the image they did not need.
		if g.SlotKey != "" {
			key := g.SlotKey
			row.SlotKey = &key
		}
		out = append(out, row)
	}
	return out
}

// idList guarantees a non-nil slice, so an empty list renders as [] rather than null.
func idList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

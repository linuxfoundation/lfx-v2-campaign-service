// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Both wire shapes decode, because both are live: every config stored before LFXV2-2665 holds a
// bare string, and CampaignCreateInput.config is declared Any, so nothing upstream rewrote them.
// A decoder that accepted only the object form would refuse every existing brief.
func TestGoogleAdsImageRef_UnmarshalAcceptsBothShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want googleAdsImageRefs
	}{
		"bare strings are URLs": {
			in:   `["https://cdn.example.org/a.png","https://cdn.example.org/b.png"]`,
			want: googleAdsImageRefs{{URL: "https://cdn.example.org/a.png"}, {URL: "https://cdn.example.org/b.png"}},
		},
		"objects carry the asset id": {
			in:   `[{"imageAssetId":"11111111-1111-4111-8111-111111111111"}]`,
			want: googleAdsImageRefs{{ImageAssetID: "11111111-1111-4111-8111-111111111111"}},
		},
		"an object may carry a url instead": {
			in:   `[{"url":"https://cdn.example.org/a.png"}]`,
			want: googleAdsImageRefs{{URL: "https://cdn.example.org/a.png"}},
		},
		"the two shapes mix in one slot": {
			in:   `["https://cdn.example.org/a.png",{"imageAssetId":"22222222-2222-4222-8222-222222222222"}]`,
			want: googleAdsImageRefs{{URL: "https://cdn.example.org/a.png"}, {ImageAssetID: "22222222-2222-4222-8222-222222222222"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got googleAdsImageRefs
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A URL-only ref must encode back to the BARE STRING it arrived as. googleAdsSnapshotConfig
// re-marshals the config to persist it, so encoding a URL as {"url":"..."} would silently
// rewrite the shape of every existing snapshot on its next write — a migration nobody asked
// for, performed by a sanitizer.
func TestGoogleAdsImageRef_MarshalKeepsTheShapeItArrivedIn(t *testing.T) {
	const in = `["https://cdn.example.org/a.png",{"imageAssetId":"11111111-1111-4111-8111-111111111111"}]`
	var refs googleAdsImageRefs
	if err := json.Unmarshal([]byte(in), &refs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != in {
		t.Errorf("round trip changed the wire shape:\n got %s\nwant %s", out, in)
	}
}

// Every path that asks "is this entry asset-backed" must answer the same way, or the snapshot
// records a shape no other path can produce. A whitespace-only id is URL-backed everywhere else —
// the resolver trims it, so it is never canonicalized and never resolved — so the encoder must
// not write it as an object carrying an imageAssetId key that the dangling-reference guard then
// skips.
func TestGoogleAdsImageRef_MarshalAgreesWithNamesAsset(t *testing.T) {
	refs := googleAdsImageRefs{{URL: "https://cdn.example.org/a.png", ImageAssetID: "   "}}
	if refs[0].namesAsset() {
		t.Fatal("a whitespace-only id must not read as asset-backed")
	}
	out, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `["https://cdn.example.org/a.png"]`
	if string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}

// The sanitizer reduces a URL — config_snapshot is persisted UNENCRYPTED and a signed query
// string IS the credential — but must leave an asset id ALONE. The id is this service's own
// primary key, and it is the exact value checkNewSnapshotAssetRefs reads back out of the
// snapshot: reducing it would make the stored reference unresolvable and the guard blind.
func TestSanitizeSnapshotImageRefs_ReducesURLsAndLeavesAssetIDs(t *testing.T) {
	const assetID = "11111111-1111-4111-8111-111111111111"
	in := googleAdsImageRefs{
		{URL: "https://cdn.example.org/a.png?X-Amz-Signature=s3cr3t-asset-key"},
		{ImageAssetID: assetID},
	}
	got := sanitizeSnapshotImageRefs(in)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if strings.Contains(got[0].URL, "s3cr3t") {
		t.Errorf("the signing query survived: %q", got[0].URL)
	}
	if !strings.HasPrefix(got[0].URL, "https://cdn.example.org") {
		t.Errorf("the host was lost: %q", got[0].URL)
	}
	if got[1].ImageAssetID != assetID {
		t.Errorf("the asset id was rewritten to %q; checkNewSnapshotAssetRefs would no longer find it", got[1].ImageAssetID)
	}
	// A NEW slice, or sanitizing would write through the backing array the fetch is about to
	// download from — the failure the deep copy exists to prevent.
	if !strings.Contains(in[0].URL, "s3cr3t") {
		t.Error("the caller's entry was mutated; the signed url must still be fetchable")
	}
}

// slotEntries is the translation at the dispatch/platform boundary: the platform client works in
// flat per-slot string lists, so an asset becomes one token carrying its id. A URL must pass
// through byte for byte — the client fetches exactly what the caller wrote.
func TestGoogleAdsImageRefs_SlotEntries(t *testing.T) {
	const assetID = "11111111-1111-4111-8111-111111111111"
	got := googleAdsImageRefs{
		{URL: "https://cdn.example.org/a.png?sig=keep-me"},
		{ImageAssetID: assetID},
	}.slotEntries()
	want := []string{"https://cdn.example.org/a.png?sig=keep-me", googleAdsImageRefScheme + assetID}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if googleAdsImageRefs(nil).slotEntries() != nil {
		t.Error("an empty slot must render as nil, not an empty list")
	}
}

// The token prefix is a CONTRACT between the two halves: dispatch writes the entry and the map
// key, the platform client matches them. Spelled twice it can drift, and the drift is silent —
// the client would fetch "lfx-asset:<uuid>" over HTTP instead of reading the bytes beside it.
func TestGoogleAdsImageRefScheme_IsThePlatformConstant(t *testing.T) {
	if googleAdsImageRefScheme == "" {
		t.Fatal("the scheme is empty; every entry would render as a bare uuid")
	}
	// The scheme is reserved against caller entries by resolveGoogleCreativeAssets, not by the
	// https rule — but a web scheme would still be the one spelling that could not be reserved
	// without refusing ordinary creative URLs.
	if strings.HasPrefix(googleAdsImageRefScheme, "http") {
		t.Errorf("the scheme %q is a web scheme and could collide with a caller URL", googleAdsImageRefScheme)
	}
}

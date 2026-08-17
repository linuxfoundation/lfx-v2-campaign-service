// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCreativeImageDropsResolvedBytesOnMarshal pins the json:"-" contract on CreativeImage:
// the dispatcher-resolved image Bytes/MIME/Checksum are NEVER serialized. That keeps a
// marshalled CampaignInput (e.g. a logged/snapshotted request) from carrying a
// multi-megabyte image blob, and keeps a config body from injecting raw bytes — the same
// guarantee meta.AdVariant.ImageBytes makes. AssetID (the config field the dispatcher
// resolves FROM) does serialize, so a snapshot still records which asset was referenced.
func TestCreativeImageDropsResolvedBytesOnMarshal(t *testing.T) {
	img := CreativeImage{
		AssetID:  "asset-123",
		Bytes:    []byte("SECRETIMAGEBYTES"),
		MIME:     "image/png",
		Checksum: "deadbeefchecksum",
	}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal CreativeImage: %v", err)
	}
	s := string(b)
	for _, leaked := range []string{"SECRETIMAGEBYTES", "deadbeefchecksum", "image/png"} {
		if strings.Contains(s, leaked) {
			t.Errorf("resolved image field %q must not serialize (json:\"-\"), got %s", leaked, s)
		}
	}
	if !strings.Contains(s, "asset-123") {
		t.Errorf("AssetID should serialize so a snapshot records the referenced asset, got %s", s)
	}
}

// TestDemandGenCreativeMarshalCarriesNoImageBytes extends the same guarantee up one level:
// a whole DemandGenCreative with all three image roles resolved to bytes marshals with no
// image bytes anywhere, so a CampaignInput.Creative snapshot is safe to log.
func TestDemandGenCreativeMarshalCarriesNoImageBytes(t *testing.T) {
	c := DemandGenCreative{
		MediaFormat:          MediaFormatSingleImage,
		BusinessName:         "Linux Foundation",
		MarketingImage:       CreativeImage{AssetID: "m", Bytes: []byte("LANDSCAPE_BYTES"), MIME: "image/png", Checksum: "cm"},
		SquareMarketingImage: CreativeImage{AssetID: "s", Bytes: []byte("SQUARE_BYTES"), MIME: "image/jpeg", Checksum: "cs"},
		Logo:                 CreativeImage{AssetID: "l", Bytes: []byte("LOGO_BYTES"), MIME: "image/png", Checksum: "cl"},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal DemandGenCreative: %v", err)
	}
	s := string(b)
	for _, leaked := range []string{"LANDSCAPE_BYTES", "SQUARE_BYTES", "LOGO_BYTES"} {
		if strings.Contains(s, leaked) {
			t.Errorf("image bytes for role must not serialize, leaked %q in %s", leaked, s)
		}
	}
}

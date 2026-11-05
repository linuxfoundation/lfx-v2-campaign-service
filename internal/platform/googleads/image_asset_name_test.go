// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Google refuses an image asset with no name (NAME_REQUIRED_FOR_ASSET_TYPE, #491). The name
// is the fixed prefix and 16 hex characters of the bytes' SHA-256: deterministic, within
// Google's 128-character limit, and free of anything the caller supplied as an address.
func TestImageAssetName(t *testing.T) {
	a := imageAssetName([]byte("first image"))
	if !regexp.MustCompile(`^LFX image [0-9a-f]{16}$`).MatchString(a) {
		t.Fatalf("imageAssetName = %q, want %q followed by 16 lower-case hex characters", a, imageAssetNamePrefix)
	}
	if len(a) > 128 {
		t.Errorf("imageAssetName is %d characters; Google refuses a name over 128", len(a))
	}
	if b := imageAssetName([]byte("first image")); b != a {
		t.Errorf("imageAssetName is not deterministic: %q then %q", a, b)
	}
	if b := imageAssetName([]byte("second image")); b == a {
		t.Errorf("two different images share the name %q", a)
	}
	// A known vector, so a change to the hash or the slice length cannot pass silently:
	// SHA-256("abc") begins ba7816bf8f01cfea.
	if got, want := imageAssetName([]byte("abc")), "LFX image ba7816bf8f01cfea"; got != want {
		t.Errorf("imageAssetName(abc) = %q, want %q", got, want)
	}
}

// assertImageAssetsNamed checks every image asset create in a mutate body — assets:mutate's
// "operations" or googleAds:mutate's "mutateOperations" — carries imageAssetName of the very
// bytes it sends, and that at least one image asset was found.
func assertImageAssetsNamed(t *testing.T, body string) {
	t.Helper()
	type create struct {
		Name       string `json:"name"`
		ImageAsset *struct {
			Data string `json:"data"`
		} `json:"imageAsset"`
	}
	var req struct {
		Operations []struct {
			Create create `json:"create"`
		} `json:"operations"`
		MutateOperations []struct {
			AssetOperation *struct {
				Create create `json:"create"`
			} `json:"assetOperation"`
		} `json:"mutateOperations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decoding the mutate body: %v", err)
	}
	creates := make([]create, 0, len(req.Operations)+len(req.MutateOperations))
	for _, op := range req.Operations {
		creates = append(creates, op.Create)
	}
	for _, op := range req.MutateOperations {
		if op.AssetOperation != nil {
			creates = append(creates, op.AssetOperation.Create)
		}
	}
	images := 0
	for i, c := range creates {
		if c.ImageAsset == nil {
			continue
		}
		images++
		data, err := base64.StdEncoding.DecodeString(c.ImageAsset.Data)
		if err != nil {
			t.Fatalf("image asset %d data is not base64: %v", i, err)
		}
		if want := imageAssetName(data); c.Name != want {
			t.Errorf("image asset %d name = %q, want %q: Google refuses an image asset with no name", i, c.Name, want)
		}
		if strings.Contains(c.Name, "://") {
			t.Errorf("image asset %d name %q carries a URL", i, c.Name)
		}
	}
	if images == 0 {
		t.Fatalf("no image asset create in the mutate body: %.300s", body)
	}
}

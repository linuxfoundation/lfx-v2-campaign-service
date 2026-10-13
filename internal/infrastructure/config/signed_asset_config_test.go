// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package config

import (
	"strings"
	"testing"
)

// The signing key is a secret: every formatted form of the config must mask it, while still
// reporting WHETHER it is set (LFXV2-2665).
func TestStringMasksCreativeAssetSigningKey(t *testing.T) {
	const key = "c2VjcmV0LXNpZ25pbmcta2V5LXRoYXQtaXMtbG9uZy1lbm91Z2g="
	c := &Config{
		CreativeAssetURLSigningKey: key,
		CreativeAssetPublicBaseURL: "https://user:pw@lfx-api.example.org",
		CreativeAssetURLTTL:        "10m",
	}
	for _, s := range []string{c.String(), c.GoString()} {
		if strings.Contains(s, key) {
			t.Fatalf("config string leaks the signing key: %s", s)
		}
		if strings.Contains(s, "pw@") {
			t.Fatalf("config string leaks base-URL userinfo: %s", s)
		}
		if !strings.Contains(s, `CreativeAssetURLSigningKey:"xxxxx"`) || !strings.Contains(s, `CreativeAssetURLTTL:"10m"`) {
			t.Fatalf("config string does not report the signed-asset settings: %s", s)
		}
	}
}

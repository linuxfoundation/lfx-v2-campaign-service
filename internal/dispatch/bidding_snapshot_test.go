// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// snapshotLane decodes one platform's config, returns its bidding, and builds the config_snapshot
// the platform's own campaignFrom* mapper persists.
type snapshotLane struct {
	key string
	run func(t *testing.T, config json.RawMessage) (*biddingConfig, json.RawMessage)
}

func snapshotLanes() []snapshotLane {
	ctx := context.Background()
	return []snapshotLane{
		{"microsoftConfig", func(t *testing.T, c json.RawMessage) (*biddingConfig, json.RawMessage) {
			var cfg microsoftConfig
			mustDecode(t, c, "microsoftConfig", &cfg)
			return cfg.Bidding, campaignFromMicrosoft(ctx, &microsoft.CampaignResult{}, cfg).ConfigSnapshot
		}},
		{"metaConfig", func(t *testing.T, c json.RawMessage) (*biddingConfig, json.RawMessage) {
			var cfg metaConfig
			mustDecode(t, c, "metaConfig", &cfg)
			return cfg.Bidding, campaignFromMeta(ctx, &meta.CampaignResult{}, cfg).ConfigSnapshot
		}},
		{"redditConfig", func(t *testing.T, c json.RawMessage) (*biddingConfig, json.RawMessage) {
			var cfg redditConfig
			mustDecode(t, c, "redditConfig", &cfg)
			return cfg.Bidding, campaignFromReddit(ctx, &reddit.CampaignResult{}, cfg).ConfigSnapshot
		}},
		{"twitterConfig", func(t *testing.T, c json.RawMessage) (*biddingConfig, json.RawMessage) {
			var cfg twitterConfig
			mustDecode(t, c, "twitterConfig", &cfg)
			return cfg.Bidding, campaignFromTwitter(ctx, &twitter.CampaignResult{}, cfg).ConfigSnapshot
		}},
		{"linkedInConfig", func(t *testing.T, c json.RawMessage) (*biddingConfig, json.RawMessage) {
			var cfg linkedinConfig
			mustDecode(t, c, "linkedInConfig", &cfg)
			return cfg.Bidding, campaignFromLinkedIn(ctx, &linkedin.CampaignResult{}, 0, cfg).ConfigSnapshot
		}},
	}
}

func mustDecode(t *testing.T, config json.RawMessage, key string, dst any) {
	t.Helper()
	if err := unmarshalPlatformConfig(config, key, dst); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
}

// TestConfigSnapshot_BiddingRoundTripsThroughTheValidator (PR #332 review): a stored config_snapshot
// must be a valid, replayable config. The default encoding wrote `"bidding":null` for an omitted
// choice and `"strategy":""`/`"bidAmount":null` for an automatic object — exactly what the validator
// now refuses. For each platform, the snapshot of an omitted, an automatic and a manual bidding
// config re-decodes through checkBiddingShape and resolveBidding to the SAME plan.
func TestConfigSnapshot_BiddingRoundTripsThroughTheValidator(t *testing.T) {
	bodies := map[string]string{
		"omitted":      ``,
		"empty object": `,"bidding":{}`,
		"automatic":    `,"bidding":{"strategy":"automatic"}`,
		"manual":       `,"bidding":{"strategy":"manual","bidAmount":1.25}`,
	}
	for _, lane := range snapshotLanes() {
		for name, bidding := range bodies {
			t.Run(lane.key+"/"+name, func(t *testing.T) {
				config := json.RawMessage(`{"` + lane.key + `":{"budget":50` + bidding + `}}`)
				original, snapshot := lane.run(t, config)
				want, err := resolveBidding(lane.key, original)
				if err != nil {
					t.Fatalf("original config: %v", err)
				}
				for _, bad := range []string{`"bidding":null`, `"strategy":""`, `"bidAmount":null`} {
					if strings.Contains(string(snapshot), bad) {
						t.Fatalf("snapshot %s carries %s, which the validator refuses", snapshot, bad)
					}
				}
				replay := json.RawMessage(`{"` + lane.key + `":` + string(snapshot) + `}`)
				if err := checkBiddingShape(replay, lane.key); err != nil {
					t.Fatalf("the stored snapshot fails the pre-check: %v (snapshot %s)", err, snapshot)
				}
				replayed, _ := lane.run(t, replay)
				got, err := resolveBidding(lane.key, replayed)
				if err != nil || got != want {
					t.Fatalf("replayed plan = %+v, %v; want %+v (snapshot %s)", got, err, want, snapshot)
				}
			})
		}
	}
}

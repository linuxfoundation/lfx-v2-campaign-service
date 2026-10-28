// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// biddingUnsupportedConfigKeys names the per-platform config keys whose dispatcher does NOT
// implement the create-time `bidding` object (LFXV2-2665). Google Ads has its own, richer bidding
// fields (googleAdsConfig.biddingStrategy and friends) and is outside this change, so a `bidding`
// object there would be silently ignored by its decoder — a caller who asked for manual bidding
// would get a campaign bidding however biddingStrategy says, while believing otherwise. It is
// refused here, on the synchronous create path, so that never happens.
var biddingUnsupportedConfigKeys = map[model.Provider]string{
	model.ProviderGoogleAds: "googleAdsConfig",
}

// refuseUnsupportedBidding answers 400 when a selected platform without bidding support carries a
// `bidding` object in its config. The message names both Google fields that replace it,
// biddingStrategy and cpcBid, so a caller after a manual bid finds the bid field too (#156). Only the key's PRESENCE is read, so a config that does not decode
// is left to the dispatcher, which reports it as the job failure exactly as before.
func refuseUnsupportedBidding(platforms []model.Provider, config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(config, &envelope); err != nil {
		return nil
	}
	for _, p := range platforms {
		key, ok := biddingUnsupportedConfigKeys[p]
		if !ok {
			continue
		}
		var platformCfg map[string]json.RawMessage
		if err := json.Unmarshal(envelope[key], &platformCfg); err != nil {
			continue
		}
		if _, present := platformCfg["bidding"]; present {
			return &briefs.BadRequestError{Code: "400", Message: fmt.Sprintf(
				"invalid campaign config: %s.bidding is not supported for %s; set its bid strategy with %s.biddingStrategy and a manual max CPC bid with %s.cpcBid instead", key, p, key, key)}
		}
	}
	return nil
}

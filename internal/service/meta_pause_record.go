// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// metaPauseRecord is one intended change to a Meta campaign row's deliberate-pause record: the
// ad-set lever's (LFXV2-2665) or, since #550 gap 14, the per-ad lever's. The record writes and
// their reconciliation (writeMetaAdSetRecord and its helpers) are shared by both levers; only
// what is written, and how a row is recognised as carrying it, differ.
type metaPauseRecord struct {
	// apply returns result with the intended change made.
	apply func(result json.RawMessage) (json.RawMessage, error)
	// holds reports whether result already carries the intended change — the proof a reconciled
	// commit landed.
	holds func(result json.RawMessage) bool
	// actor is the UpdatedBy operation name of the write.
	actor string
}

// adSetPauseRecord sets the ad-set record to adSetID, or clears it when adSetID is "".
func adSetPauseRecord(adSetID string) metaPauseRecord {
	return metaPauseRecord{
		apply: func(r json.RawMessage) (json.RawMessage, error) { return model.WithMetaUserPausedAdSet(r, adSetID) },
		holds: func(r json.RawMessage) bool { return model.MetaUserPausedAdSetID(r) == adSetID },
		actor: "toggle meta ad set status",
	}
}

// adPauseRecord adds adID to the per-ad record (paused) or removes it (!paused).
func adPauseRecord(adID string, paused bool) metaPauseRecord {
	return metaPauseRecord{
		apply: func(r json.RawMessage) (json.RawMessage, error) { return model.WithMetaUserPausedAd(r, adID, paused) },
		holds: func(r json.RawMessage) bool { return model.MetaAdHeldPaused(r, adID) == paused },
		actor: "toggle meta ad status",
	}
}

// metaLeverNoun names the object a lever addresses in its fixed client messages: one ("ad set",
// "ad") and the read that lists them (many: "ad sets", "ads").
type metaLeverNoun struct{ one, many string }

var (
	adSetLeverNoun = metaLeverNoun{one: "ad set", many: "ad sets"}
	adLeverNoun    = metaLeverNoun{one: "ad", many: "ads"}
)

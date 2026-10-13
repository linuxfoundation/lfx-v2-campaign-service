// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"errors"
	"strings"
)

// A Meta campaign row's Result blob is the create path's meta.CampaignResult, marshalled
// UNTAGGED, so its keys are Go field names. Two of them are read here, by a package that must not
// import the platform client:
//
//   - MetaResultAdSetIDKey is meta.CampaignResult.AdSetID — the ad set this service created
//     (dispatch.metaAdSetID reads the same key through the struct; a dispatch test pins that the
//     two agree).
//   - MetaResultUserPausedAdSetKey is NOT a CampaignResult field. It records a DELIBERATE pause of
//     that ad set made through toggle-meta-ad-set-status (LFXV2-2665, product decision
//     2026-10-13): while it names the recorded ad set, a campaign ACTIVATE leaves that ad set
//     PAUSED instead of cascading ACTIVE onto it. It is written only by the ad-set lever and
//     removed only by a CONFIRMED resume through that lever. It lives in the existing JSON
//     column rather than a new one because it is Meta-only and the shared campaigns table has
//     no other ad-set notion (the reasoning dispatch.metaAdSetID records for the ad set id).
//     A re-dispatch writes a fresh Result, and with it a fresh ad set, so the flag cannot
//     outlive the ad set it describes.
const (
	MetaResultAdSetIDKey         = "AdSetID"
	MetaResultUserPausedAdSetKey = "UserPausedAdSetID"
)

// ErrMetaResultNotObject is returned by WithMetaUserPausedAdSet when the stored Result is not a
// JSON object, so the flag cannot be written without discarding what the blob holds.
var ErrMetaResultNotObject = errors.New("the campaign's result blob is not a JSON object")

// metaPauseView decodes the two keys this file reads. encoding/json matches keys
// case-insensitively, exactly as the dispatcher's decode into meta.CampaignResult does, so the two
// readers cannot disagree about which key a blob carries.
type metaPauseView struct {
	AdSetID           string
	UserPausedAdSetID string
}

func decodeMetaPauseView(result json.RawMessage) metaPauseView {
	var v metaPauseView
	if len(result) == 0 || json.Unmarshal(result, &v) != nil {
		return metaPauseView{}
	}
	return v
}

// MetaRecordedAdSetID is the ad set id the create path recorded in result, or "" when the blob
// records none (an adopted campaign, a degraded create) or cannot be decoded.
func MetaRecordedAdSetID(result json.RawMessage) string {
	return decodeMetaPauseView(result).AdSetID
}

// MetaUserPausedAdSetID is the ad set id a deliberate pause is on record for, or "".
func MetaUserPausedAdSetID(result json.RawMessage) string {
	return decodeMetaPauseView(result).UserPausedAdSetID
}

// MetaAdSetHeldPaused reports whether adSetID is the campaign's recorded ad set AND a deliberate
// pause of it is on record — the one case in which a campaign ACTIVATE leaves it PAUSED.
func MetaAdSetHeldPaused(result json.RawMessage, adSetID string) bool {
	v := decodeMetaPauseView(result)
	return adSetID != "" && v.AdSetID == adSetID && v.UserPausedAdSetID == adSetID
}

// WithMetaUserPausedAdSet returns result with the deliberate-pause record set to adSetID, or
// removed when adSetID is "". Every other key is preserved verbatim (re-encoded as an object, so
// key ORDER is not preserved; nothing reads it). An empty result starts from an empty object.
func WithMetaUserPausedAdSet(result json.RawMessage, adSetID string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &fields); err != nil || fields == nil {
			return nil, ErrMetaResultNotObject
		}
	}
	// Remove every case variant first: the readers match case-insensitively, so a stray variant
	// left behind could still be read as the record.
	for k := range fields {
		if strings.EqualFold(k, MetaResultUserPausedAdSetKey) {
			delete(fields, k)
		}
	}
	if adSetID != "" {
		raw, err := json.Marshal(adSetID)
		if err != nil {
			return nil, err
		}
		fields[MetaResultUserPausedAdSetKey] = raw
	}
	return json.Marshal(fields)
}

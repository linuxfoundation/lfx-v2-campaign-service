// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// AdoptedResultKey is the key in a campaign row's Result blob that records the row was bound
// by adopt-campaign — a campaign that already existed on the ad platform — rather than created
// by this service. It is the SINGLE source of the public `adopted` flag (the campaign result
// type and the indexed CampaignDoc both read it through CampaignAdopted).
//
// LinkedIn's adopter has written it since adoption was added there (linkedin.CampaignResult's
// Adopted field). BriefService.AdoptCampaign now stamps it on EVERY platform's blob through
// MarkResultAdopted, so the adapters need not each remember to. A row adopted before that
// stamp existed on any other platform carries no key and reads as NOT adopted: its blob says
// so only in a prose Steps line, and prose is not parsed for a flag a client branches on.
const AdoptedResultKey = "adopted"

// CampaignAdopted reports whether the row was bound by adoption: its Result blob carries
// `"adopted": true`. An empty, unreadable or non-object blob reads as false — the flag is
// additive information, never a guard, so an unreadable blob must not invent it.
func CampaignAdopted(c *Campaign) bool {
	if c == nil || len(c.Result) == 0 {
		return false
	}
	var blob struct {
		Adopted bool `json:"adopted"`
	}
	if err := json.Unmarshal(c.Result, &blob); err != nil {
		return false
	}
	return blob.Adopted
}

// MarkResultAdopted returns raw with `"adopted": true` set at its top level, every other key
// preserved verbatim. An empty blob becomes `{"adopted":true}`. A blob that is not a JSON
// object is an adapter contract violation and is returned as an error rather than replaced:
// overwriting it would discard the account provenance the mismatch guards read back.
func MarkResultAdopted(raw json.RawMessage) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("mark adopted: the result blob is not a JSON object: %w", err)
		}
		if fields == nil {
			// A literal `null` unmarshals into a nil map; treat it as empty.
			fields = map[string]json.RawMessage{}
		}
	}
	fields[AdoptedResultKey] = json.RawMessage("true")
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("mark adopted: %w", err)
	}
	return out, nil
}

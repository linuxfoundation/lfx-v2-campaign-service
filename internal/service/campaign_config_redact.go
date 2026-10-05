// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
)

// redactedConfigSnapshot renders a caller-supplied campaign config for persistence in
// campaigns.config_snapshot with every embedded link redacted.
//
// config_snapshot is stored UNENCRYPTED and is carried into the campaign index document, so
// nothing a caller types may land there with a credential intact. On the create path each
// dispatch adapter builds the snapshot from its own validated struct and scrubs the fields it
// knows carry links. UpdateCampaign has no adapter in the loop: `config` is Goa `Any`, so ANY
// JSON the caller sends is persisted as-is, and a per-provider field list cannot cover it.
// This is therefore provider-agnostic: the value is walked recursively and every STRING
// value goes through redact.SnapshotText, the same full redactor the adapters use for free
// text (scheme-ful URLs → scheme+host, scheme-less links with a query/fragment or a path →
// host, userinfo runs dropped). Object keys are not rewritten — they are the shape of the
// config, not caller content worth a credential — and numbers, booleans and null pass
// through untouched.
//
// Numbers are decoded with UseNumber so the redaction round trip writes back exactly the
// digits marshalAny produced and adds no float64 reformatting of its own (Goa's `Any` decode
// already carries a JSON number as float64 before this runs; that is unchanged). A value
// that cannot be marshaled yields nil, which is what marshalAny already returned on that
// path.
func redactedConfigSnapshot(config any) json.RawMessage {
	raw := marshalAny(config)
	if raw == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		// Unreachable for a value json.Marshal just produced; fail closed rather than
		// persist the unredacted bytes.
		return nil
	}
	return marshalAny(redactSnapshotValue(decoded))
}

// redactSnapshotValue returns v with every string value run through redact.SnapshotText.
// It rebuilds containers rather than mutating them in place.
func redactSnapshotValue(v any) any {
	switch t := v.(type) {
	case string:
		return redact.SnapshotText(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = redactSnapshotValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactSnapshotValue(val)
		}
		return out
	default:
		// json.Number, bool, nil.
		return v
	}
}

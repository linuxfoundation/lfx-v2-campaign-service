// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"

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
// host, userinfo runs dropped). Object KEYS go through the same redactor: `config` is
// caller-typed all the way down, so a map keyed by, say, landing-page URL carries its links
// in the keys. Numbers, booleans and null pass through untouched. See redactSnapshotObject
// for what happens when two keys redact to the same string.
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

// redactSnapshotValue returns v with every string value and every object key run through
// redact.SnapshotText. It rebuilds containers rather than mutating them in place.
func redactSnapshotValue(v any) any {
	switch t := v.(type) {
	case string:
		return redact.SnapshotText(t)
	case map[string]any:
		return redactSnapshotObject(t)
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

// redactSnapshotObject redacts an object's keys and values.
//
// Redacting keys can make two of them equal — `https://a.example/x?t=1` and
// `https://a.example/y` both become `https://a.example` — and a map cannot hold both. Values
// are never silently merged or dropped on a collision: the original keys are processed in
// sorted order, the first to produce a redacted key keeps it, and each later one is stored
// under `<redacted>#2`, `#3`, … (the lowest suffix not already taken). Sorting first makes the
// output stable across runs regardless of Go's map iteration order.
func redactSnapshotObject(in map[string]any) map[string]any {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(in))
	for _, k := range keys {
		rk := redact.SnapshotText(k)
		if _, taken := out[rk]; taken {
			for n := 2; ; n++ {
				candidate := rk + "#" + strconv.Itoa(n)
				if _, taken := out[candidate]; !taken {
					rk = candidate
					break
				}
			}
		}
		out[rk] = redactSnapshotValue(in[k])
	}
	return out
}

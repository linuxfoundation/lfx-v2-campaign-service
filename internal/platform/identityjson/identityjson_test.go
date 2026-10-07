// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package identityjson

import (
	"errors"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		refuse bool
	}{
		{"plain object", `{"id":"1","name":"KubeCon"}`, false},
		{"a genuine U+FFFD is a legal name", `{"name":"bad�name","id":"1"}`, false},
		{"a valid surrogate pair", `{"name":"😀"}`, false},
		{"a doubled backslash is literal text", `{"name":"\\uD800"}`, false},
		{"same key in sibling objects", `{"a":{"id":"1"},"b":{"id":"2"}}`, false},
		{"same key across array elements", `[{"id":"1"},{"id":"2"}]`, false},
		{"malformed JSON is left to the decoder", `{"id":`, false},
		{"duplicate key", `{"id":"1","id":"2"}`, true},
		{"duplicate key differing only in case", `{"id":"1","ID":"2"}`, true},
		{"duplicate key spelled with KELVIN SIGN", `{"kid":"1","Kid":"2"}`, true},
		{"duplicate key nested in an array", `{"data":[{"name":"a","name":"b"}]}`, true},
		{"unpaired high surrogate", `{"name":"bad\uD800name"}`, true},
		{"lone low surrogate", `{"name":"bad\uDC00name"}`, true},
		{"malformed UTF-8 byte", "{\"name\":\"bad\xffname\"}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check([]byte(tc.raw))
			if tc.refuse != (err != nil) {
				t.Fatalf("Check(%q) = %v, want refused=%v", tc.raw, err, tc.refuse)
			}
			if err != nil && !errors.Is(err, ErrUntrustworthy) {
				t.Errorf("refusal %v does not wrap ErrUntrustworthy", err)
			}
		})
	}
}

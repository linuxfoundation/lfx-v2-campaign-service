// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package identityjson refuses a JSON response that cannot be trusted as IDENTITY evidence:
// one that encoding/json would decode WITHOUT error into something other than what the bytes
// say.
//
// It exists for campaign adoption (adopt-campaign, LFXV2-2665). An adoption binds a brief to an
// arbitrary upstream campaign on the strength of one read, and the name and id that read
// returns are what an operator confirms the binding against. encoding/json has three silent
// behaviours that turn a malformed answer into a plausible one:
//
//   - malformed UTF-8 bytes inside a string are replaced with U+FFFD, with no error;
//   - an unpaired surrogate escape (`\uD800`) — six ASCII bytes, valid UTF-8 — is likewise
//     replaced with U+FFFD;
//   - a key repeated inside one object (or repeated under the decoder's case-insensitive
//     field match) is resolved in favour of the LAST value, so one object can carry two ids
//     and the decoder picks one.
//
// The Google Ads client applies the same three guards to its adoption read
// (internal/platform/googleads/campaign_lookup.go: hasDuplicateKeys,
// hasUnpairedSurrogateEscape). They are reproduced here, rather than imported, because those
// helpers are unexported in a package whose owners did not take on this change; the two should
// converge on this package when that file next moves.
package identityjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrUntrustworthy marks a response this package refused. Its text is this package's own
// sentence and never carries a byte of the response.
var ErrUntrustworthy = errors.New("the response cannot be trusted as identity evidence")

// Check returns nil when raw can be decoded without any silent substitution, and an error
// wrapping ErrUntrustworthy otherwise. Malformed JSON is NOT reported here: the caller's own
// json.Unmarshal reports that, with the better diagnostic.
func Check(raw []byte) error {
	switch {
	case !utf8.Valid(raw):
		return errors.Join(ErrUntrustworthy, errors.New("it carries malformed UTF-8 bytes, which decoding would silently replace with U+FFFD"))
	case hasUnpairedSurrogateEscape(raw):
		return errors.Join(ErrUntrustworthy, errors.New("it carries an unpaired surrogate escape, which decoding would silently replace with U+FFFD"))
	case hasDuplicateKeys(raw):
		return errors.Join(ErrUntrustworthy, errors.New("an object in it declares the same key twice, so it describes more than one value and the decoder would silently pick one"))
	}
	return nil
}

// hasDuplicateKeys reports whether any object in b declares the same key twice, under the
// decoder's own notion of sameness (foldKey). The whole document is walked, not only the
// fields a caller reads: a duplicate anywhere is evidence the producer is not emitting what the
// caller thinks it is. Malformed JSON returns false (see Check).
func hasDuplicateKeys(b []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	// One frame per still-open container: the keys seen so far in an object, nil for an array.
	var stack []map[string]struct{}
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, map[string]struct{}{})
				expectKey = true
			case '[':
				stack = append(stack, nil)
				expectKey = false
			case '}', ']':
				if len(stack) == 0 {
					return false
				}
				stack = stack[:len(stack)-1]
				expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
			}
		case string:
			if expectKey {
				seen := stack[len(stack)-1]
				k := foldKey(t)
				if _, dup := seen[k]; dup {
					return true
				}
				seen[k] = struct{}{}
				expectKey = false
				continue
			}
			expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
		default:
			expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
		}
	}
}

// foldKey maps a key onto the form encoding/json matches struct fields by: case-insensitive,
// with KELVIN SIGN and LATIN SMALL LETTER LONG S folding to 'k' and 's' as the decoder's
// simple fold does. Every platform this serves uses one case convention throughout, so no
// legitimate object carries two keys differing only in case.
func foldKey(k string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		switch r {
		case '\u212A': // KELVIN SIGN
			return 'k'
		case '\u017F': // LATIN SMALL LETTER LONG S
			return 's'
		}
		return r
	}, k))
}

// hasUnpairedSurrogateEscape reports whether b contains a \uD800-\uDFFF escape that is not part
// of a valid high+low pair. A doubled backslash is literal data and starts no escape.
func hasUnpairedSurrogateEscape(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		if i+1 < len(b) && b[i+1] == '\\' {
			i++
			continue
		}
		if i+5 >= len(b) || b[i+1] != 'u' {
			continue
		}
		r, ok := hex4(b[i+2 : i+6])
		if !ok {
			continue // malformed hex: json.Unmarshal reports it
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			if i+11 >= len(b) || b[i+6] != '\\' || b[i+7] != 'u' {
				return true
			}
			lo, ok := hex4(b[i+8 : i+12])
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 11
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		default:
			i += 5
		}
	}
	return false
}

// hex4 parses exactly four hex digits, the fixed width of a JSON \u escape.
func hex4(b []byte) (uint32, bool) {
	if len(b) != 4 {
		return 0, false
	}
	var v uint32
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

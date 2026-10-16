// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
)

const (
	redacted = "[REDACTED]"
	// minSecretLen keeps a short credential value (none is legitimately this short) from
	// scrubbing ordinary text it happens to match.
	minSecretLen = 6
)

// secretKeyRe matches a JSON key, query parameter or header name whose VALUE is a secret.
// Matched by name so a secret this process never saw (a fresh token in an upstream body, a
// signature in a download URL) is still withheld.
var secretKeyRe = regexp.MustCompile(`(?i)(token|secret|password|passwd|signature|^sig$|authorization|api[_-]?key|credential|private|client[_-]?id|developer|cookie)`)

// oauthParamRe pulls the quoted values out of an OAuth 1.0a Authorization header (X Ads).
var oauthParamRe = regexp.MustCompile(`="([^"]+)"`)

// secretSet is every secret value this run has observed: the decrypted credential document,
// the Authorization-style headers sent upstream, and the tokens a token exchange returned.
// Every byte this command prints passes through scrub.
type secretSet struct {
	mu     sync.Mutex
	values map[string]struct{}
}

func newSecretSet() *secretSet { return &secretSet{values: map[string]struct{}{}} }

func (s *secretSet) add(v string) {
	v = strings.TrimSpace(v)
	if len(v) < minSecretLen {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[v] = struct{}{}
	// The same value as it appears in a query string or form body.
	if e := url.QueryEscape(v); e != v {
		s.values[e] = struct{}{}
	}
	// And as it appears inside a JSON string, with and without HTML escaping: a credential with
	// a quote or backslash echoed in an upstream body is printed escaped (PR #362 review).
	for _, e := range jsonEscapings(v) {
		if e != v {
			s.values[e] = struct{}{}
		}
	}
}

// jsonEscapings returns v as the body of a JSON string literal, as encoding/json writes it with
// and without HTML escaping.
func jsonEscapings(v string) []string {
	out := make([]string, 0, 2)
	if b, err := json.Marshal(v); err == nil {
		out = append(out, string(b[1:len(b)-1]))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		b := bytes.TrimRight(buf.Bytes(), "\n")
		out = append(out, string(b[1:len(b)-1]))
	}
	return out
}

// addCredentialDocument records every string value of a decrypted credential document. A
// document that is not JSON is recorded whole.
func (s *secretSet) addCredentialDocument(plaintext []byte) {
	var doc any
	if err := json.Unmarshal(plaintext, &doc); err != nil {
		s.add(string(plaintext))
		return
	}
	s.addJSONStrings(doc)
}

func (s *secretSet) addJSONStrings(v any) {
	switch t := v.(type) {
	case string:
		s.add(t)
	case map[string]any:
		for _, e := range t {
			s.addJSONStrings(e)
		}
	case []any:
		for _, e := range t {
			s.addJSONStrings(e)
		}
	}
}

// issuedTokenKeyRe names the fields of a token exchange response that carry a credential
// (access_token, refresh_token, id_token) — not token_type or scope, whose values ("bearer")
// are ordinary words that would otherwise be scrubbed from everything printed.
var issuedTokenKeyRe = regexp.MustCompile(`(?i)token$`)

// addIssuedTokens records the credential values of a token exchange response.
func (s *secretSet) addIssuedTokens(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if issuedTokenKeyRe.MatchString(k) {
				s.addJSONStrings(e)
				continue
			}
			s.addIssuedTokens(e)
		}
	case []any:
		for _, e := range t {
			s.addIssuedTokens(e)
		}
	}
}

// addHeaderValue records a credential-bearing header: the whole value, the part after an auth
// scheme ("Bearer x"), and each quoted OAuth 1.0a parameter.
func (s *secretSet) addHeaderValue(v string) {
	s.add(v)
	if _, after, ok := strings.Cut(v, " "); ok {
		s.add(after)
	}
	for _, m := range oauthParamRe.FindAllStringSubmatch(v, -1) {
		s.add(m[1])
		if u, err := url.QueryUnescape(m[1]); err == nil {
			s.add(u)
		}
	}
}

// scrub replaces every recorded secret in text, longest first so a secret that contains
// another is not left half-replaced.
func (s *secretSet) scrub(text string) string {
	s.mu.Lock()
	vals := make([]string, 0, len(s.values))
	for v := range s.values {
		vals = append(vals, v)
	}
	s.mu.Unlock()
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		text = strings.ReplaceAll(text, v, redacted)
	}
	return text
}

// scrubWriter is the only way this command writes output.
type scrubWriter struct {
	w       io.Writer
	secrets *secretSet
}

func (w *scrubWriter) printf(format string, a ...any) {
	_, _ = io.WriteString(w.w, w.secrets.scrub(fmt.Sprintf(format, a...)))
}

// displayURL renders a request URL for output: pkg/redact.URL (userinfo, query and fragment
// dropped), then the query re-attached parameter by parameter with any secret-named value
// withheld. The query is worth keeping — X sends its whole stats request there — but a signed
// download URL (Microsoft's report file) carries its signature there too.
func displayURL(u *url.URL) string {
	base := redact.URL(u)
	if u == nil || u.RawQuery == "" {
		return base
	}
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range q[k] {
			if secretKeyRe.MatchString(k) {
				v = redacted
			}
			parts = append(parts, url.QueryEscape(k)+"="+v)
		}
	}
	return base + "?" + strings.Join(parts, "&")
}

// redactBody renders a captured body for output. JSON is re-indented with every secret-named
// key's value withheld and every recorded secret scrubbed from each DECODED string — before
// re-encoding can escape a quote or backslash in it out of the secret set's reach (PR #362
// review). Every string also has the URLs in it reduced by pkg/redact.SnapshotText, the rule the
// service applies before persisting free text, so a signed URL in a body (Microsoft's poll
// response names the report file's) cannot leak its signature. A body that looks like JSON but
// does not decode whole — cut off by the capture cap or an interrupted read — is WITHHELD: as
// raw text, an escaped `\u0026` would end SnapshotText's URL run before a later `sig=`. Any
// other text is passed through SnapshotText. The secret-set scrub also runs on everything
// printed.
func redactBody(body []byte, secrets *secretSet) string {
	// UseNumber: a raw dump must not round an int64 id or count through float64.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err == nil && dec.Decode(new(any)) == io.EOF {
		// Rendered WITHOUT HTML escaping: an escaped `&` (\u0026) ends a URL run for
		// SnapshotText, which would then reduce only the part before it and leave a later
		// `sig=` in the text. redactJSON also reduces URLs on the decoded strings, before any
		// encoding can split them.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if enc.Encode(redactJSON(doc, secrets)) == nil {
			return redact.SnapshotText(strings.TrimRight(buf.String(), "\n"))
		}
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		return withheldIncompleteJSON
	}
	return redact.SnapshotText(string(body))
}

// withheldIncompleteJSON stands in for a JSON-looking body that did not decode whole.
const withheldIncompleteJSON = "(withheld: a JSON body that did not decode whole — an escape cut off mid-value could carry a secret past redaction)"

func redactJSON(v any, secrets *secretSet) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if secretKeyRe.MatchString(k) {
				out[k] = redacted
				continue
			}
			out[k] = redactJSON(e, secrets)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactJSON(e, secrets)
		}
		return out
	case string:
		t = redact.SnapshotText(t)
		if secrets != nil {
			t = secrets.scrub(t)
		}
		return t
	}
	return v
}

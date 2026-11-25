// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// A newsletter issue's links come from one place: the operator's notes (the brief's source
// document). The newsletter prompt says so, but a prompt is an instruction, not a guarantee, and
// a newsletter is the one email whose whole purpose is its links. So the rule is enforced here,
// on the model's answer: an anchor is kept only when its address is one the notes carry.
//
// The comparison is on the WHOLE address, not its host. A host rule would pass an invented path
// on a real site ("https://example.org/blog/a-post-that-does-not-exist"), which is exactly the
// link a model writes when it wants one and was given none.

// sourceLinkPattern finds the http(s) addresses in free text. It stops at whitespace and at the
// characters that close a quotation or a tag, never inside an address. Case-insensitive on the
// scheme: a pasted "HTTPS://" is still an address.
var sourceLinkPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

// trailingLinkPunctuation is what prose puts after an address without meaning it as part of it:
// "see https://example.org/x." or "(https://example.org/x)".
const trailingLinkPunctuation = ".,;:!?)]}>"

// normalizeLinkForMatch is the form two addresses are compared in: scheme and host lower-cased,
// and an empty path read as "/" (RFC 3986 makes the two the same address). "" for anything that
// is not an absolute http(s) address with a hostname, and for one that carries userinfo. It
// deliberately keeps the path's case, a trailing slash, the query and the fragment: a server can
// serve a different page at "/report/" than at "/report", and a fragment the notes did not give
// sends the reader to a section the operator did not choose. A link that differs from the notes
// in any of them loses its anchor, which is the safe way to be wrong.
func normalizeLinkForMatch(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	// No userinfo: "https://user:secret@host/" is a credential in a link every recipient would
	// receive, and "https://trusted.example@evil.example/" reads as one site and opens another.
	// Hostname, not Host: ":8080" alone is a Host with no site in it.
	if err != nil || u.Hostname() == "" || u.User != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	u.Scheme, u.Host = scheme, strings.ToLower(u.Host)
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	return u.String()
}

// trimTrailingLinkPunctuation drops the punctuation prose puts after an address. A closing
// bracket that closes one opened INSIDE the address belongs to it (".../wiki/Foo_(bar)"); only an
// unmatched one is the prose's ("(see https://example.org/x)"). The UI's extractHttpLinks applies
// the same rule, and the two must agree: a link kept there and cut here loses its anchor.
func trimTrailingLinkPunctuation(raw string) string {
	for raw != "" && strings.ContainsRune(trailingLinkPunctuation, rune(raw[len(raw)-1])) {
		if raw[len(raw)-1] == ')' && strings.Count(raw, "(") >= strings.Count(raw, ")") {
			break
		}
		raw = raw[:len(raw)-1]
	}
	return raw
}

// sourceDocumentLinks returns every address the notes carry, in match form.
func sourceDocumentLinks(sourceText string) map[string]struct{} {
	links := map[string]struct{}{}
	for _, raw := range sourceLinkPattern.FindAllString(sourceText, -1) {
		if key := normalizeLinkForMatch(trimTrailingLinkPunctuation(raw)); key != "" {
			links[key] = struct{}{}
		}
	}
	return links
}

// unlinkUnvouchedAnchors rewrites html so that only anchors pointing at an allowed address keep
// their href. Any other anchor keeps its words and loses every attribute, so it reads as the
// plain text it effectively is. Everything that is not such an anchor is returned byte for byte.
// The second result counts the anchors that lost their link.
func unlinkUnvouchedAnchors(body string, allowed map[string]struct{}) (string, int) {
	if !strings.Contains(strings.ToLower(body), "<a") {
		return body, 0
	}
	z := html.NewTokenizer(strings.NewReader(body))
	var b strings.Builder
	dropped := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		// Copied before the tag is read: TagName and TagAttr may rewrite the tokenizer's buffer.
		raw := string(z.Raw())
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			b.WriteString(raw)
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) != "a" {
			b.WriteString(raw)
			continue
		}
		href := ""
		for hasAttr {
			var k, v []byte
			k, v, hasAttr = z.TagAttr()
			if string(k) == "href" {
				href = string(v)
			}
		}
		if _, ok := allowed[normalizeLinkForMatch(href)]; ok && href != "" {
			b.WriteString(raw)
			continue
		}
		dropped++
		b.WriteString("<a>")
	}
	return b.String(), dropped
}

// enforceSourceLinks applies the rule to a generated newsletter: every rich-text anchor and the
// button's destination must be an address the notes carry. It returns how many links were
// removed, for the log. A button that loses its destination keeps its label, which the caller
// renders as text, the same answer every stage gives a button with nowhere to go.
func enforceSourceLinks(copy *briefs.EmailCopy, sourceText string) int {
	if copy == nil {
		return 0
	}
	allowed := sourceDocumentLinks(sourceText)
	dropped := 0
	for _, section := range copy.Sections {
		if section == nil {
			continue
		}
		if section.HTML != nil {
			rewritten, n := unlinkUnvouchedAnchors(*section.HTML, allowed)
			section.HTML = &rewritten
			dropped += n
		}
		if section.URL != nil {
			if _, ok := allowed[normalizeLinkForMatch(*section.URL)]; !ok {
				section.URL = nil
				dropped++
			}
		}
	}
	return dropped
}

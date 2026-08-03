// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utm

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// DefaultLinkPrefix labels body links in utm_content ("body-link-1", "body-link-2", …), so a
// report can tell which link in an email was clicked.
const DefaultLinkPrefix = "body-link"

// TagHTMLLinks rewrites every <a href> in an HTML fragment to carry the UTM parameters,
// numbering utm_content as "<prefix>-N" in document order.
//
// It returns the input UNCHANGED when there is nothing to do (blank HTML, no campaign) or when
// the fragment cannot be parsed — email body HTML comes from a marketing tool and may be
// fragmentary or malformed, and returning a mangled body would break the email itself. An
// untagged email is a reporting gap; a broken one is a failed send.
//
// Links that Apply declines to tag (mailto:/tel:/anchors, already-tagged links) are left alone
// AND do not consume a number, so the numbering describes the links that were actually tagged
// rather than counting skipped ones.
func TagHTMLLinks(fragment string, p Params, prefix string) (string, error) {
	if strings.TrimSpace(fragment) == "" || strings.TrimSpace(p.Campaign) == "" {
		return fragment, nil
	}
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultLinkPrefix
	}

	// ParseFragment (not Parse): email bodies are fragments, and Parse would wrap them in a
	// synthesized html/head/body, changing the content the caller sends.
	// The context node's DataAtom MUST match its Data, or ParseFragment rejects it as an
	// "inconsistent Node" and every call silently returns the fragment untagged.
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{
		Type:     html.ElementNode,
		Data:     "body",
		DataAtom: atom.Body,
	})
	if err != nil {
		return fragment, fmt.Errorf("utm: parse email html: %w", err)
	}

	var n int
	for _, root := range nodes {
		tagAnchors(root, p, prefix, &n)
	}

	var b strings.Builder
	for _, root := range nodes {
		if rerr := html.Render(&b, root); rerr != nil {
			// Render failing after a successful parse would mean emitting a truncated body;
			// return the original instead.
			return fragment, fmt.Errorf("utm: render email html: %w", rerr)
		}
	}
	return b.String(), nil
}

// tagAnchors walks the tree and rewrites anchor hrefs in place. n is the running count of
// links actually TAGGED, so utm_content numbering skips links that were left alone.
func tagAnchors(node *html.Node, p Params, prefix string, n *int) {
	if node.Type == html.ElementNode && node.Data == "a" {
		for i := range node.Attr {
			if !strings.EqualFold(node.Attr[i].Key, "href") {
				continue
			}
			href := node.Attr[i].Val
			// Compute the candidate content label from the count this link WOULD take, then
			// only consume the number if the link was actually changed.
			candidate := fmt.Sprintf("%s-%d", prefix, *n+1)
			if tagged := Apply(href, p, candidate); tagged != href {
				node.Attr[i].Val = tagged
				*n++
			}
			break // an anchor has at most one href
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		tagAnchors(child, p, prefix, n)
	}
}

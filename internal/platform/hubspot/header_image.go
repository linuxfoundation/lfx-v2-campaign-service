// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// HeaderImage is the image a drag-and-drop email opens with: its source, alt text and link.
type HeaderImage struct {
	Src  string
	Alt  string
	Link string
}

// LeadingHeaderImage returns the image module that sits ABOVE an email draft's first rich-text
// block, in the layout's reading order, and whether there is one. Read-only.
//
// It exists for the newsletter surface. RebuildEmailContent replaces the whole widget tree, so a
// clone keeps a header only when the caller supplies a hero image; a newsletter brief has none,
// and the brand's logo header -- the one part of the previous issue that is not stale -- was
// dropped. The caller re-uses this image as the hero. Only an image BEFORE the first text block
// counts: an image further down is part of last issue's content, not the brand's header.
func (c *Client) LeadingHeaderImage(ctx context.Context, id string) (HeaderImage, bool, error) {
	if id = strings.TrimSpace(id); id == "" {
		return HeaderImage{}, false, fmt.Errorf("hubspot: LeadingHeaderImage requires a non-empty id")
	}
	raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"/"+url.PathEscape(id)+"/draft", nil, true)
	if err != nil {
		return HeaderImage{}, false, err
	}
	var ec emailContent
	if uerr := json.Unmarshal(raw, &ec); uerr != nil {
		return HeaderImage{}, false, fmt.Errorf("hubspot: decode email %s draft content: %w", id, uerr)
	}
	img, ok := ec.leadingHeaderImage()
	return img, ok, nil
}

func (ec emailContent) leadingHeaderImage() (HeaderImage, bool) {
	for _, key := range ec.layoutOrder() {
		rawWidget, ok := ec.Content.Widgets[key]
		if !ok {
			continue
		}
		var w widgetBody
		if json.Unmarshal(rawWidget, &w) != nil {
			continue
		}
		if _, isRichText := w.html(); isRichText {
			return HeaderImage{}, false
		}
		rawImg, hasImg := w.Body["img"]
		if !hasImg {
			continue
		}
		var img struct {
			Src string `json:"src"`
			Alt string `json:"alt"`
		}
		if json.Unmarshal(rawImg, &img) != nil || strings.TrimSpace(img.Src) == "" {
			continue
		}
		link := ""
		if rawLink, hasLink := w.Body["link"]; hasLink {
			_ = json.Unmarshal(rawLink, &link)
		}
		return HeaderImage{Src: strings.TrimSpace(img.Src), Alt: strings.TrimSpace(img.Alt), Link: strings.TrimSpace(link)}, true
	}
	return HeaderImage{}, false
}

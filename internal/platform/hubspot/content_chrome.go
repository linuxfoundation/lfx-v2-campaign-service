// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// ErrNoBrandChrome reports that a draft carries no footer this client recognises (no social
// "follow" module and no email-footer module placed in its layout), so there is no header and
// footer to keep around new content. Nothing was written. A caller that still wants the content
// applied falls back to RebuildEmailContent, which writes the standard footer.
var ErrNoBrandChrome = errors.New("hubspot: the email has no footer modules to keep around new content")

// ReplaceEmailBodyInput is what ReplaceEmailBodyKeepingChrome writes between a draft's own header
// and footer.
type ReplaceEmailBodyInput struct {
	// BodyHTML is the new content. It is written as one rich-text module per top-level <h2>
	// section, so each item of a newsletter can be moved, edited or deleted on its own in the
	// HubSpot editor. Inline `style` attributes are dropped: the draft's own style settings (the
	// brand's fonts, heading sizes and link colour) then apply, as they do to text typed there.
	BodyHTML string
	// ButtonText and ButtonURL add one button after the content. Empty ButtonURL adds none.
	ButtonText string
	ButtonURL  string
	// PreviewText is the inbox preheader. Empty keeps the draft's own.
	PreviewText string
}

// KeptChrome reports what ReplaceEmailBodyKeepingChrome kept and wrote.
type KeptChrome struct {
	// HeaderImage is the image kept above the content, nil when the draft opens with none.
	HeaderImage *HeaderImage
	// HeaderSections and FooterSections count the layout sections kept as they were.
	HeaderSections int
	FooterSections int
	// ContentModules counts the rich-text modules written between them.
	ContentModules int
	// ContentKeys are the widget keys this call wrote, the button included, in reading order.
	ContentKeys []string
}

// Widget kinds, as far as keeping a draft's header and footer needs to tell modules apart.
type widgetKind int

const (
	kindOther widgetKind = iota
	kindRichText
	kindImage
	kindButton
	kindDivider
	kindSocial
	kindFooter
)

// chromeWidget is one widget decoded far enough to classify it. HubSpot writes a module's path
// and id inside its body; some editors also (or only) write them on the widget itself.
type chromeWidget struct {
	ModuleID json.Number                `json:"module_id"`
	Path     string                     `json:"path"`
	Body     map[string]json.RawMessage `json:"body"`
}

func (w chromeWidget) path() string {
	var p string
	if raw, ok := w.Body["path"]; ok {
		_ = json.Unmarshal(raw, &p)
	}
	if p == "" {
		p = w.Path
	}
	return strings.ToLower(p)
}

func (w chromeWidget) moduleID() string {
	if raw, ok := w.Body["module_id"]; ok {
		var n json.Number
		if json.Unmarshal(raw, &n) == nil && n.String() != "" {
			return n.String()
		}
	}
	return w.ModuleID.String()
}

// kind classifies a widget by what it is, read from its module path first and its body shape
// second: modules placed by older editors carry no path at all, only a module id and a body.
func (w chromeWidget) kind() widgetKind {
	p, id := w.path(), w.moduleID()
	switch {
	case strings.Contains(p, "email_footer") || id == fmt.Sprint(moduleIDEmailFooter):
		return kindFooter
	case strings.Contains(p, "follow_me") || id == fmt.Sprint(moduleIDFollowMe):
		return kindSocial
	}
	if _, ok := w.Body["social"]; ok {
		return kindSocial
	}
	if raw, ok := w.Body["html"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return kindRichText
		}
	}
	if _, ok := w.Body["img"]; ok {
		return kindImage
	}
	if _, ok := w.Body["destination"]; ok || strings.Contains(p, "button") || id == fmt.Sprint(moduleIDButton) {
		return kindButton
	}
	if _, ok := w.Body["line_type"]; ok || strings.Contains(p, "divider") || id == fmt.Sprint(moduleIDEmailDivider) {
		return kindDivider
	}
	return kindOther
}

func (w chromeWidget) html() string {
	var s string
	if raw, ok := w.Body["html"]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// isStandardRichText reports whether the widget is HubSpot's own rich-text module, the one whose
// text takes the email's style settings. A raw-HTML module also carries `html`, but it renders
// its markup as written, so it is no template for new prose.
func (w chromeWidget) isStandardRichText() bool {
	if w.kind() != kindRichText {
		return false
	}
	if p := w.path(); p != "" {
		return strings.Contains(p, "rich_text")
	}
	return w.moduleID() == fmt.Sprint(moduleIDRichText)
}

// chromeSection is one layout section: its raw JSON, kept byte for byte when the section is
// kept, and the widget keys it places.
type chromeSection struct {
	raw     json.RawMessage
	widgets []string
	style   json.RawMessage
}

// followLeadIn matches the short heading a footer opens with ("Follow Us", "Connect with us!").
var followLeadIn = regexp.MustCompile(`(?i)\b(follow|connect with|stay connected|stay in touch|join the conversation|find us)\b`)

// maxFollowLeadInRunes bounds the text of a section counted as the footer's lead-in. The longest
// seen in the LF portal is one sentence under a "Follow Us" heading, about 180 characters.
const maxFollowLeadInRunes = 300

// maxContentModules bounds how many rich-text modules one body is split into. Further headings
// stay in the last module rather than being dropped.
const maxContentModules = 40

// ReplaceEmailBodyKeepingChrome replaces the CONTENT of an email draft and keeps its header and
// footer: the sections above the first text block (a view-in-browser line, the brand's logo or
// banner) and the trailing run that starts at the first social or footer module (the follow-us
// heading, social links, "sent by" line and the footer module). Everything between them is
// removed and the new content written in its place. The draft's template path, style settings
// and each kept section's own style are sent back unchanged. MUTATING, non-idempotent.
//
// It exists for a recurring newsletter, where the draft is a clone of the brand's previous issue.
// RebuildEmailContent replaces the whole tree and writes the standard Linux Foundation Events
// footer, which put LF's social links and "sent by" line under another brand's newsletter.
//
// The read and the write are two requests, with the lost-update window SetEmailHTMLWidgets
// documents. The write is verified by verifyContentSaved, exactly as a full rebuild is.
//
// ErrNoBrandChrome is returned, with nothing written, when the draft has no footer to keep.
func (c *Client) ReplaceEmailBodyKeepingChrome(ctx context.Context, id string, in ReplaceEmailBodyInput) (*Email, KeptChrome, error) {
	var kept KeptChrome
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, kept, fmt.Errorf("hubspot: ReplaceEmailBodyKeepingChrome requires a non-empty id")
	}
	chunks := splitAtTopLevelHeadings(stripInlineStyles(stripStagedMarkers(in.BodyHTML)))
	if len(chunks) == 0 {
		return nil, kept, fmt.Errorf("hubspot: ReplaceEmailBodyKeepingChrome requires a non-empty body")
	}

	current, err := c.readDraftContent(ctx, id)
	if err != nil {
		return nil, kept, fmt.Errorf("hubspot: read email %s draft before replacing its content: %w", id, err)
	}
	var flex map[string]json.RawMessage
	if uerr := json.Unmarshal(current.Content["flexAreas"], &flex); uerr != nil || len(flex) == 0 {
		return nil, kept, fmt.Errorf("%w: email %s places no modules", ErrNoBrandChrome, id)
	}
	areaName := mainAreaName(flex)
	var area map[string]json.RawMessage
	if uerr := json.Unmarshal(flex[areaName], &area); uerr != nil {
		return nil, kept, fmt.Errorf("hubspot: decode email %s layout: %w", id, uerr)
	}
	var rawSections []json.RawMessage
	if uerr := json.Unmarshal(area["sections"], &rawSections); uerr != nil {
		return nil, kept, fmt.Errorf("hubspot: decode email %s sections: %w", id, uerr)
	}
	var rawWidgets map[string]json.RawMessage
	if uerr := json.Unmarshal(current.Content["widgets"], &rawWidgets); uerr != nil {
		return nil, kept, fmt.Errorf("hubspot: decode email %s widgets: %w", id, uerr)
	}

	widgets := make(map[string]chromeWidget, len(rawWidgets))
	for key, raw := range rawWidgets {
		var w chromeWidget
		if json.Unmarshal(raw, &w) == nil {
			widgets[key] = w
		}
	}
	sections := make([]chromeSection, 0, len(rawSections))
	for _, raw := range rawSections {
		sections = append(sections, decodeChromeSection(raw))
	}

	footerStart := footerStartIndex(sections, widgets)
	if footerStart < 0 {
		return nil, kept, fmt.Errorf("%w: email %s", ErrNoBrandChrome, id)
	}
	headerEnd, headerImage := headerEndIndex(sections[:footerStart], widgets)
	content := sections[headerEnd:footerStart]

	// What the new modules are modelled on: the previous issue's first rich-text module (its
	// padding and module settings), the style of the section it sat in (the content area's
	// background), and its first button. Each falls back to this client's defaults.
	textTemplate, sectionStyle, buttonTemplate := contentTemplates(content, rawWidgets, widgets)

	newWidgets := map[string]json.RawMessage{}
	placed := map[string]any{}
	keep := func(secs []chromeSection) {
		for _, s := range secs {
			for _, key := range s.widgets {
				if raw, ok := rawWidgets[key]; ok {
					newWidgets[key] = raw
					placed[key] = nil
				}
			}
		}
	}
	keep(sections[:headerEnd])
	keep(sections[footerStart:])

	newSections := make([]json.RawMessage, 0, len(sections)+len(chunks)+1)
	for _, s := range sections[:headerEnd] {
		newSections = append(newSections, s.raw)
	}

	if len(chunks) > maxContentModules {
		chunks = append(chunks[:maxContentModules-1], strings.Join(chunks[maxContentModules-1:], "\n"))
	}
	bodyToken := ""
	for i, chunk := range chunks {
		key, sectionID, columnID := bodyWidgetKey, "section-staging-body", "col-body-0"
		if i > 0 {
			key = fmt.Sprintf("%s_%d", bodyWidgetKey, i+1)
			sectionID = fmt.Sprintf("section-staging-body-%d", i+1)
			columnID = fmt.Sprintf("col-body-%d", i)
		}
		if i == 0 {
			// The marker rides the first module, under the key verifyContentSaved reads.
			marker := stagedBodyMarker(in.BodyHTML)
			bodyToken = strings.TrimSuffix(strings.TrimPrefix(marker, "<!-- "), " -->")
			chunk = strings.TrimRight(chunk, " \t\r\n") + "\n" + marker
		}
		rawWidget, werr := richTextWidgetLike(textTemplate, chunk)
		if werr != nil {
			return nil, kept, fmt.Errorf("hubspot: build email %s content module: %w", id, werr)
		}
		newWidgets[key] = rawWidget
		placed[key] = nil
		kept.ContentKeys = append(kept.ContentKeys, key)
		sec, merr := marshalSection(sectionID, columnID, key, sectionStyle)
		if merr != nil {
			return nil, kept, fmt.Errorf("hubspot: build email %s content section: %w", id, merr)
		}
		newSections = append(newSections, sec)
	}
	kept.ContentModules = len(chunks)

	if dest := strings.TrimSpace(in.ButtonURL); dest != "" {
		const key = "staging_button"
		rawButton, berr := buttonWidgetLike(buttonTemplate, in.ButtonText, dest)
		if berr != nil {
			return nil, kept, fmt.Errorf("hubspot: build email %s button: %w", id, berr)
		}
		newWidgets[key] = rawButton
		placed[key] = nil
		kept.ContentKeys = append(kept.ContentKeys, key)
		sec, merr := marshalSection("section-staging-button", "col-button-0", key, sectionStyle)
		if merr != nil {
			return nil, kept, fmt.Errorf("hubspot: build email %s button section: %w", id, merr)
		}
		newSections = append(newSections, sec)
	}

	for _, s := range sections[footerStart:] {
		newSections = append(newSections, s.raw)
	}

	if text := strings.TrimSpace(in.PreviewText); text != "" {
		raw, _ := json.Marshal(map[string]any{"body": map[string]any{"value": text}})
		newWidgets["preview_text"] = raw
	} else if raw, ok := rawWidgets["preview_text"]; ok {
		newWidgets["preview_text"] = raw
	}

	encodedSections, err := json.Marshal(newSections)
	if err != nil {
		return nil, kept, fmt.Errorf("hubspot: encode email %s sections: %w", id, err)
	}
	area["sections"] = encodedSections
	encodedArea, err := json.Marshal(area)
	if err != nil {
		return nil, kept, fmt.Errorf("hubspot: encode email %s layout: %w", id, err)
	}
	// Only the area that was read is rebuilt. A second flex area has never been observed on a
	// drag-and-drop email; were one present, its modules would no longer be in the widget map,
	// so it is dropped rather than left pointing at them.
	newContent := map[string]json.RawMessage{}
	for k, v := range current.Content {
		newContent[k] = v
	}
	encodedWidgets, err := json.Marshal(newWidgets)
	if err != nil {
		return nil, kept, fmt.Errorf("hubspot: encode email %s widgets: %w", id, err)
	}
	encodedFlex, err := json.Marshal(map[string]json.RawMessage{areaName: encodedArea})
	if err != nil {
		return nil, kept, fmt.Errorf("hubspot: encode email %s flex areas: %w", id, err)
	}
	newContent["widgets"] = encodedWidgets
	newContent["flexAreas"] = encodedFlex

	email, err := c.patchEmail(ctx, id, map[string]any{"content": newContent})
	if err != nil {
		return nil, kept, err
	}
	kept.HeaderImage = headerImage
	kept.HeaderSections = headerEnd
	kept.FooterSections = len(sections) - footerStart

	if verr := c.verifyContentSaved(ctx, id, placed, bodyToken); verr != nil {
		if errors.Is(verr, errVerifyReadFailed) {
			return email, kept, verr
		}
		return email, kept, fmt.Errorf("%w: %w", ErrContentNotPersisted, verr)
	}
	return email, kept, nil
}

// mainAreaName picks the flex area to rebuild: `main`, else the first in name order.
func mainAreaName(flex map[string]json.RawMessage) string {
	if _, ok := flex["main"]; ok {
		return "main"
	}
	names := make([]string, 0, len(flex))
	for name := range flex {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0]
}

func decodeChromeSection(raw json.RawMessage) chromeSection {
	var s struct {
		Columns []struct {
			Widgets []string `json:"widgets"`
		} `json:"columns"`
		Style json.RawMessage `json:"style"`
	}
	_ = json.Unmarshal(raw, &s)
	out := chromeSection{raw: raw, style: s.Style}
	for _, col := range s.Columns {
		out.widgets = append(out.widgets, col.Widgets...)
	}
	return out
}

// footerStartIndex returns the index of the first section of the footer run, or -1 when the
// layout places neither a social nor a footer module.
//
// The run starts at the first section holding one of those two modules, and reaches back over
// the sections directly above it that are only its lead-in: a short follow-us heading, or a
// divider. A longer text block, an image or a button above the social links is content.
func footerStartIndex(sections []chromeSection, widgets map[string]chromeWidget) int {
	start := -1
	for i, s := range sections {
		for _, key := range s.widgets {
			if k := widgets[key].kind(); k == kindSocial || k == kindFooter {
				start = i
				break
			}
		}
		if start >= 0 {
			break
		}
	}
	for start > 0 && isFooterLeadIn(sections[start-1], widgets) {
		start--
	}
	return start
}

func isFooterLeadIn(s chromeSection, widgets map[string]chromeWidget) bool {
	if len(s.widgets) == 0 {
		return false
	}
	text := ""
	for _, key := range s.widgets {
		w := widgets[key]
		switch w.kind() {
		case kindDivider:
		case kindRichText:
			text += " " + visibleText(w.html())
		default:
			return false
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return true // dividers only
	}
	return utf8.RuneCountInString(text) <= maxFollowLeadInRunes && followLeadIn.MatchString(text)
}

// headerEndIndex returns how many leading sections are the header, and the header's image.
//
// A header section holds no text, button or divider: it is a view-in-browser line or other
// template module, or the one image the email opens with. A second image section is content.
func headerEndIndex(sections []chromeSection, widgets map[string]chromeWidget) (int, *HeaderImage) {
	var image *HeaderImage
	for i, s := range sections {
		hasImage := false
		for _, key := range s.widgets {
			switch widgets[key].kind() {
			case kindRichText, kindButton, kindDivider, kindSocial, kindFooter:
				return i, image
			case kindImage:
				hasImage = true
			}
		}
		if !hasImage {
			continue
		}
		if image != nil {
			return i, image
		}
		for _, key := range s.widgets {
			if w := widgets[key]; w.kind() == kindImage {
				image = headerImageOf(w)
				break
			}
		}
		if image == nil {
			image = &HeaderImage{}
		}
	}
	return len(sections), image
}

func headerImageOf(w chromeWidget) *HeaderImage {
	var img struct {
		Src string `json:"src"`
		Alt string `json:"alt"`
	}
	_ = json.Unmarshal(w.Body["img"], &img)
	link := ""
	if raw, ok := w.Body["link"]; ok {
		_ = json.Unmarshal(raw, &link)
	}
	return &HeaderImage{Src: strings.TrimSpace(img.Src), Alt: strings.TrimSpace(img.Alt), Link: strings.TrimSpace(link)}
}

// contentTemplates finds, in the content being replaced, the modules the new ones are modelled
// on. textTemplate and buttonTemplate are nil when the content has none of that kind;
// sectionStyle is nil when no text section carried a style.
func contentTemplates(content []chromeSection, raw map[string]json.RawMessage, widgets map[string]chromeWidget) (textTemplate, sectionStyle, buttonTemplate json.RawMessage) {
	for _, s := range content {
		for _, key := range s.widgets {
			w := widgets[key]
			switch w.kind() {
			case kindRichText:
				if sectionStyle == nil && len(s.style) > 0 {
					sectionStyle = s.style
				}
				if textTemplate == nil && w.isStandardRichText() {
					textTemplate = raw[key]
				}
			case kindButton:
				if buttonTemplate == nil {
					buttonTemplate = raw[key]
				}
			}
		}
	}
	return textTemplate, sectionStyle, buttonTemplate
}

// richTextWidgetLike returns a rich-text module holding htmlBody: the template with its html
// replaced when there is one, else this client's own rich-text module.
func richTextWidgetLike(template json.RawMessage, htmlBody string) (json.RawMessage, error) {
	if len(template) > 0 {
		if w, err := widgetWithHTML(template, htmlBody); err == nil {
			return w, nil
		}
	}
	return json.Marshal(richTextWidget(htmlBody, "15px", "20px", "10px", "20px"))
}

// buttonWidgetLike returns a button module: the template with its text and destination replaced
// when there is one, so the button keeps the brand's colours, else this client's own button.
func buttonWidgetLike(template json.RawMessage, text, destination string) (json.RawMessage, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "Read More"
	}
	if len(template) > 0 {
		var w map[string]json.RawMessage
		var body map[string]json.RawMessage
		if json.Unmarshal(template, &w) == nil && w != nil && json.Unmarshal(w["body"], &body) == nil && body != nil {
			encodedText, _ := json.Marshal(text)
			encodedDest, _ := json.Marshal(destination)
			body["text"] = encodedText
			body["destination"] = encodedDest
			if encodedBody, err := json.Marshal(body); err == nil {
				w["body"] = encodedBody
				return json.Marshal(w)
			}
		}
	}
	widgets := map[string]any{}
	var sections []map[string]any
	addButtonSection(widgets, &sections, text, destination)
	return json.Marshal(widgets["staging_button"])
}

func marshalSection(sectionID, columnID, widgetKey string, style json.RawMessage) (json.RawMessage, error) {
	section := map[string]any{
		"id":      sectionID,
		"columns": []map[string]any{{"id": columnID, "widgets": []string{widgetKey}, "width": 12}},
	}
	if len(style) > 0 {
		section["style"] = style
	} else {
		section["style"] = defaultSectionStyle
	}
	return json.Marshal(section)
}

// voidElements never take an end tag, so they do not open a level.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

// splitAtTopLevelHeadings cuts a body into the pieces that become separate modules: a new piece
// starts at each <h2> that is not nested in another element, and at each top-level <hr>, which
// is dropped (the module boundary is the divider). Pieces with no visible text are dropped. A
// body with no top-level <h2> is one piece.
func splitAtTopLevelHeadings(body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	z := html.NewTokenizer(strings.NewReader(body))
	var pieces []string
	var cur strings.Builder
	flush := func() {
		piece := strings.TrimSpace(cur.String())
		cur.Reset()
		if piece != "" && visibleText(piece) != "" {
			pieces = append(pieces, piece)
		}
	}
	depth := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		raw := string(z.Raw())
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			nameBytes, _ := z.TagName()
			name := string(nameBytes)
			if depth == 0 && (name == "h2" || name == "hr") {
				flush()
				if name == "hr" {
					continue
				}
			}
			cur.WriteString(raw)
			if tt == html.StartTagToken && !voidElements[name] {
				depth++
			}
		case html.EndTagToken:
			cur.WriteString(raw)
			if depth > 0 {
				depth--
			}
		default:
			cur.WriteString(raw)
		}
	}
	flush()
	return pieces
}

// stripInlineStyles removes every `style` attribute and leaves the rest of the markup as it was.
// The generated body arrives dressed in this service's own palette; inside a brand's cloned
// newsletter that palette overrides the brand's fonts and link colour, so it is taken off and
// the draft's style settings apply instead.
func stripInlineStyles(body string) string {
	if !strings.Contains(strings.ToLower(body), "style") {
		return body
	}
	z := html.NewTokenizer(strings.NewReader(body))
	var b strings.Builder
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
		nameBytes, hasAttr := z.TagName()
		name := string(nameBytes)
		type attr struct{ key, val string }
		var attrs []attr
		hadStyle := false
		for hasAttr {
			var k, v []byte
			k, v, hasAttr = z.TagAttr()
			if strings.EqualFold(string(k), "style") {
				hadStyle = true
				continue
			}
			attrs = append(attrs, attr{string(k), string(v)})
		}
		if !hadStyle {
			b.WriteString(raw)
			continue
		}
		b.WriteString("<" + name)
		for _, a := range attrs {
			b.WriteString(" " + a.key + `="` + html.EscapeString(a.val) + `"`)
		}
		if tt == html.SelfClosingTagToken {
			b.WriteString(" /")
		}
		b.WriteString(">")
	}
	return b.String()
}

// visibleText is a fragment's text with its tags removed and its whitespace collapsed.
func visibleText(fragment string) string {
	z := html.NewTokenizer(strings.NewReader(fragment))
	var b strings.Builder
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.TextToken {
			b.WriteString(html.UnescapeString(string(z.Raw())))
			b.WriteString(" ")
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

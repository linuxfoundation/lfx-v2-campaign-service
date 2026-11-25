// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// newsletterKeyword is the word a brand's newsletter carries in its HubSpot email name. The
// naming convention is "<YY>Q<N> - <Brand> - Newsletter - <Month>", and the legacy
// emailcreationskill found the clone source the same way (lookup_brand_history with the type
// hint "newsletter").
const newsletterKeyword = "newsletter"

// IsBrandNewsletter reports whether an email NAME is one of the brand's newsletters: the brand
// NewsletterBrandFromName reads from the name equals the given brand, ignoring case. Matched on
// the name only, not the subject: subjects of event emails routinely say "in our newsletter",
// while the internal name is the operator's own label for what the email is.
//
// Equality with the SAME parser that lists the brands, not containment: the brand comes from
// that listing, and containment let "LF" match the LF Education and LF Japan newsletters, so the
// wrong issue became the template. An empty brand matches nothing.
func IsBrandNewsletter(name, brand string) bool {
	brand = strings.TrimSpace(brand)
	if brand == "" {
		return false
	}
	parsed, ok := NewsletterBrandFromName(name)
	return ok && strings.EqualFold(parsed, brand)
}

// markedTestSend matches a name whose own label says the send was a test: "(test)",
// "(Marketing Ops Test)", "[TEST]". Only inside brackets, so "June Newsletter - A/B test", a real
// issue sent as an A/B test, is not one.
var markedTestSend = regexp.MustCompile(`(?i)[(\[][^)\]]*\btest\b[^)\]]*[)\]]`)

// isSentIssue reports whether an email is an ISSUE a brand sent to its subscribers: sent, by
// LastSent's gates, and neither of the two things the live LF portal also names "newsletter".
//
//   - An automated email. "22 - LF Standards - Newsletter Form" and "... - Newsletter Form - Thank
//     You" are the follow-ups a sign-up form sends, in state AUTOMATED. They listed "LF Standards"
//     and "Zephyr" as newsletter brands, and such an email has no send list to reuse.
//   - A send its own name marks as a test. "... Summer Newsletter (Marketing Ops Test)" was
//     PUBLISHED, to a test list; as the brand's latest issue it would have made a test list the
//     next issue's audience.
func isSentIssue(email hubspot.Email, now time.Time) bool {
	if !isPublished(email.State) || sentInTheFuture(string(email.PublishDate), now) {
		return false
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(email.State)), "AUTOMATED") {
		return false
	}
	return !markedTestSend.MatchString(email.Name)
}

// BrandNewsletters reports the brand's most recently SENT newsletters, newest first, with the
// lists each one targeted. The newest is the template a new issue clones; the rest are content
// references, and their lists are the precedent for the new issue's audience.
//
// It is LastSent's walk with a simpler rule: there is no event, so no tiers -- a row either is
// this brand's newsletter or it is not. The send gates are LastSent's, for LastSent's reasons:
// only a state that means the email went out counts, a booked send is not precedent, and the
// authoritative per-email date read runs again before a row is returned.
func (x *AudienceExplorer) BrandNewsletters(ctx context.Context, projectID, brand string, limit int) (sent []audience.LastSentEmail, err error) {
	brand = strings.TrimSpace(brand)
	if brand == "" {
		return []audience.LastSentEmail{}, nil
	}
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	if limit <= 0 {
		limit = 3
	}
	now := x.now()

	// The predicate is the NAME RULE ALONE and the send gates run below, for the reason
	// LastSent gives: gating inside the predicate turns "matched, but only drafts" into the
	// walk's false-absence error on a portal past the scan bound.
	emails, searchBounded, serr := client.SearchEmailsMatchingBounded(ctx, func(e hubspot.Email) bool {
		return IsBrandNewsletter(e.Name, brand)
	})
	if serr != nil {
		return nil, serr
	}

	seen := map[string]struct{}{}
	candidates := make([]ranked, 0, 8)
	for _, email := range emails {
		if _, dup := seen[email.ID]; dup {
			continue
		}
		seen[email.ID] = struct{}{}
		if !isSentIssue(email, now) {
			continue
		}
		candidates = append(candidates, ranked{email: email, sentAt: hubspot.ParseEmailTime(string(email.PublishDate))})
	}
	if searchBounded && len(candidates) == 0 {
		return nil, fmt.Errorf("brand newsletters: the email search stopped at its scan bound and every row it matched was a draft or a scheduled send: %w", hubspot.ErrSearchIncomplete)
	}

	sort.SliceStable(candidates, func(i, j int) bool { return beforeInSendOrder(candidates[i], candidates[j]) })
	shortlist := limit + shortlistHeadroom
	if shortlist > maxSendDateReads {
		shortlist = maxSendDateReads
	}
	if len(candidates) > shortlist {
		candidates = candidates[:shortlist]
	}

	shortlisted := make([]sendDateRead, 0, len(candidates))
	for _, c := range candidates {
		r := sendDateRead{row: c}
		sendLists, lerr := client.GetEmailSendLists(ctx, c.email.ID)
		if lerr != nil {
			slog.WarnContext(ctx, "could not read a brand newsletter's lists",
				"project_id", projectID, "email_id", c.email.ID, "error", lerr)
			r.unread = true
		} else {
			r.lists = sendLists
			if authoritative := hubspot.ParseEmailTime(sendLists.PublishDate); !authoritative.IsZero() {
				if authoritative.After(now) {
					continue
				}
				r.row.sentAt = authoritative
			}
		}
		shortlisted = append(shortlisted, r)
	}
	if searchBounded && len(shortlisted) == 0 {
		return nil, fmt.Errorf("brand newsletters: the email search stopped at its scan bound and every row it matched proved not to be a send: %w", hubspot.ErrSearchIncomplete)
	}

	sort.SliceStable(shortlisted, func(i, j int) bool {
		return beforeInSendOrder(shortlisted[i].row, shortlisted[j].row)
	})
	if len(shortlisted) > limit {
		shortlisted = shortlisted[:limit]
	}

	out := make([]audience.LastSentEmail, 0, len(shortlisted))
	for _, r := range shortlisted {
		row := audience.LastSentEmail{
			EmailID:          r.row.email.ID,
			EmailName:        r.row.email.Name,
			Subject:          r.row.email.Subject,
			HubSpotURL:       sendDetailsURL(client, r.row.email),
			IncludedLists:    []audience.ListBrief{},
			SuppressionLists: []audience.ListBrief{},
		}
		if r.unread {
			row.ListsUnavailable = true
		} else {
			row.IncludedLists = x.listBriefs(ctx, client, r.lists.Include, r.lists.LegacyInclude)
			row.SuppressionLists = x.listBriefs(ctx, client, r.lists.Exclude, r.lists.LegacyExclude)
		}
		if !r.row.sentAt.IsZero() {
			row.SentAt = r.row.sentAt.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	// The newest issue's links name where the brand publishes, so the next issue's offers and
	// news can be looked up there. Best-effort: one extra read, and a failure only loses the hint.
	if len(out) > 0 {
		if blocks, berr := client.GetEmailHTMLWidgets(ctx, out[0].EmailID); berr != nil {
			slog.DebugContext(ctx, "could not read the newest newsletter's content for its source site",
				"project_id", projectID, "email_id", out[0].EmailID, "error", berr)
		} else {
			out[0].SourceURL = newsletterSourceURL(blocks)
		}
	}
	return out, nil
}

// newsletterBrandsTTL is how long a project's brand listing is served from memory. Long enough
// that opening the dropdown is instant after the first read; short enough that a brand's first
// newsletter shows up the same morning.
const newsletterBrandsTTL = 15 * time.Minute

type cachedNewsletterBrands struct {
	at     time.Time
	brands []audience.NewsletterBrand
}

// quarterCode matches the "26Q2" code the LF naming convention puts first, also when it is
// glued to the next word ("26Q3HPSF").
var quarterCode = regexp.MustCompile(`^\d{2}Q\d`)

// parenthesised drops a bracketed period such as "(Oct 2026)".
var parenthesised = regexp.MustCompile(`\([^)]*\)`)

// periodWords are the words a newsletter name uses for WHEN, never for WHO: months, quarters,
// seasons and cadence. A brand is what is left once they are removed.
var periodWords = map[string]bool{
	"jan": true, "january": true, "feb": true, "february": true, "mar": true, "march": true,
	"apr": true, "april": true, "may": true, "jun": true, "june": true, "jul": true, "july": true,
	"aug": true, "august": true, "sep": true, "sept": true, "september": true, "oct": true,
	"october": true, "nov": true, "november": true, "dec": true, "december": true,
	"q1": true, "q2": true, "q3": true, "q4": true, "spring": true, "summer": true, "fall": true,
	"autumn": true, "winter": true, "edition": true, "quarterly": true, "monthly": true,
}

// isPeriodWord reports whether a word names a time rather than a brand.
func isPeriodWord(word string) bool {
	w := strings.ToLower(strings.Trim(word, ".,:;()[]'\""))
	if w == "" || periodWords[w] {
		return true
	}
	if quarterCode.MatchString(strings.ToUpper(w)) && len(w) == 4 {
		return true
	}
	if len(w) == 4 && strings.Trim(w, "0123456789") == "" {
		return true // a year
	}
	return false
}

// cleanBrand strips what a newsletter name wraps around a brand: emoji and symbols, a bracketed
// period, a leading quarter code (also glued), period words, and anything after a colon.
func cleanBrand(raw string) string {
	s := parenthesised.ReplaceAllString(raw, " ")
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	kept := make([]string, 0, 4)
	for _, word := range strings.Fields(s) {
		if loc := quarterCode.FindStringIndex(strings.ToUpper(word)); loc != nil && loc[0] == 0 && len(word) > 4 {
			word = word[4:] // "26Q3HPSF" -> "HPSF"
		}
		if isPeriodWord(word) {
			continue
		}
		kept = append(kept, word)
	}
	brand := strings.TrimSpace(strings.Trim(strings.Join(kept, " "), "-–:,_"))
	// Not a brand: a name that only MENTIONS a newsletter -- an event promo sent "(in the LF
	// Newsletter)" leaves an unbalanced bracket, and a subject-style name a "|" -- or a phrase
	// longer than any brand name in the portal (the longest real ones are three words).
	if strings.ContainsAny(brand, "|()[]") || len(strings.Fields(brand)) > maxBrandWords {
		return ""
	}
	return brand
}

// maxBrandWords is the longest brand cleanBrand accepts, in words.
const maxBrandWords = 4

// NewsletterBrandFromName extracts the brand from a newsletter's HubSpot email name, and whether
// the name is a newsletter at all.
//
// The convention is "<YY>Q<N> - <Brand> - Newsletter - <Month>", so the segment BEFORE the
// newsletter segment is the brand whenever it is not just a period ("26Q2 - PyTorch Foundation -
// November Newsletter 2026", "26Q4 - CNCF - SlashData Reports Newsletter" both name their brand
// there). Otherwise the brand is the words before "Newsletter" in its own segment ("26Q4 - LF
// Newsletter", "(Oct 2026) Valkey Community Newsletter"), cleaned of periods and symbols by
// cleanBrand. A name with nothing left is no brand.
func NewsletterBrandFromName(name string) (string, bool) {
	segments := strings.Split(name, " - ")
	for i, raw := range segments {
		seg := strings.TrimSpace(raw)
		idx := strings.Index(strings.ToLower(seg), newsletterKeyword)
		if idx < 0 {
			continue
		}
		if i > 0 {
			if prev := cleanBrand(segments[i-1]); prev != "" {
				return prev, true
			}
		}
		if brand := cleanBrand(seg[:idx]); brand != "" {
			return brand, true
		}
		return "", false
	}
	return "", false
}

// NewsletterBrands lists the brands that have SENT a newsletter in the project's portal, most
// recently sent first, for the newsletter surface's brand dropdown. Served from a per-project
// cache for newsletterBrandsTTL; see brandsCache.
//
// Only sent emails count, by the same gates as LastSent, so a brand whose only "newsletter" is a
// draft is not offered: the surface clones the previous issue, and a draft is no issue.
func (x *AudienceExplorer) NewsletterBrands(ctx context.Context, projectID string) (out []audience.NewsletterBrand, err error) {
	now := x.now()
	x.brandsMu.Lock()
	if hit, ok := x.brandsCache[projectID]; ok && now.Sub(hit.at) < newsletterBrandsTTL {
		x.brandsMu.Unlock()
		return hit.brands, nil
	}
	x.brandsMu.Unlock()

	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	emails, _, serr := client.SearchEmailsMatchingBounded(ctx, func(e hubspot.Email) bool {
		_, ok := NewsletterBrandFromName(e.Name)
		return ok
	})
	if serr != nil {
		return nil, serr
	}

	type acc struct {
		brand  audience.NewsletterBrand
		sentAt time.Time
	}
	byKey := map[string]*acc{}
	for _, email := range emails {
		if !isSentIssue(email, now) {
			continue
		}
		brand, ok := NewsletterBrandFromName(email.Name)
		if !ok {
			continue
		}
		key := strings.ToLower(brand)
		sentAt := hubspot.ParseEmailTime(string(email.PublishDate))
		cur, seen := byKey[key]
		if !seen {
			cur = &acc{brand: audience.NewsletterBrand{Brand: brand}}
			byKey[key] = cur
		}
		cur.brand.Issues++
		if !seen || sentAt.After(cur.sentAt) {
			cur.sentAt = sentAt
			cur.brand.Brand = brand
			cur.brand.LatestEmailName = email.Name
			cur.brand.LatestSentAt = ""
			if !sentAt.IsZero() {
				cur.brand.LatestSentAt = sentAt.Format(time.RFC3339)
			}
		}
	}

	accs := make([]*acc, 0, len(byKey))
	for _, a := range byKey {
		accs = append(accs, a)
	}
	sort.SliceStable(accs, func(i, j int) bool {
		if !accs[i].sentAt.Equal(accs[j].sentAt) {
			return accs[i].sentAt.After(accs[j].sentAt)
		}
		return strings.ToLower(accs[i].brand.Brand) < strings.ToLower(accs[j].brand.Brand)
	})
	out = make([]audience.NewsletterBrand, 0, len(accs))
	for _, a := range accs {
		out = append(out, a.brand)
	}

	x.brandsMu.Lock()
	if x.brandsCache == nil {
		x.brandsCache = map[string]cachedNewsletterBrands{}
	}
	x.brandsCache[projectID] = cachedNewsletterBrands{at: now, brands: out}
	x.brandsMu.Unlock()
	return out, nil
}

// hrefPattern finds the href attributes in a rich-text block.
var hrefPattern = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)

// notASourceHost lists hosts a newsletter links to that are not where the brand publishes:
// social networks, link shorteners, calendar links and HubSpot's own hosting.
var notASourceHost = []string{
	"facebook.com", "linkedin.com", "twitter.com", "x.com", "youtube.com", "youtu.be", "instagram.com",
	"github.com", "hubspot.com", "hubspotlinks.com", "hs-sites.com", "hubs.la", "hubs.ly", "bit.ly",
	"addcal.io", "mastodon.social", "bsky.app", "discord.gg", "slack.com",
}

// newsletterSourceURL is where a newsletter's brand publishes, read from the issue's links: the
// blog index when the issue links to a blog post ("https://www.linuxfoundation.org/blog"), else
// the site the issue links to most. "" when the issue carries no usable link.
func newsletterSourceURL(blocks []hubspot.EmailHTMLBlock) string {
	counts := map[string]int{}
	order := []string{}
	blog := ""
	for _, block := range blocks {
		for _, m := range hrefPattern.FindAllStringSubmatch(block.HTML, -1) {
			u, err := url.Parse(strings.TrimSpace(m[1]))
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				continue
			}
			host := strings.ToLower(u.Hostname())
			skip := false
			for _, bad := range notASourceHost {
				if host == bad || strings.HasSuffix(host, "."+bad) {
					skip = true
					break
				}
			}
			if skip || strings.HasPrefix(host, "email.") {
				continue
			}
			site := "https://" + u.Host
			if blog == "" {
				if idx := strings.Index(strings.ToLower(u.Path), "/blog"); idx >= 0 {
					blog = site + u.Path[:idx+len("/blog")]
				}
			}
			if counts[site] == 0 {
				order = append(order, site)
			}
			counts[site]++
		}
	}
	if blog != "" {
		return blog
	}
	best := ""
	for _, site := range order {
		if counts[site] > counts[best] {
			best = site
		}
	}
	return best
}

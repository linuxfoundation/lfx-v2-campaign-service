// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

func TestIsBrandNewsletter(t *testing.T) {
	cases := []struct {
		name, email, brand string
		want               bool
	}{
		{"convention name", "26Q3 - OpenSSF - Newsletter - September", "OpenSSF", true},
		{"case-insensitive", "26q3 - openssf - NEWSLETTER - sept", "OpenSSF", true},
		{"a brand is not a prefix of another", "26Q4 LF Education Newsletter - Oct 2026", "LF", false},
		{"the LF newsletter itself", "26Q4 - LF Newsletter - November 2026", "LF", true},
		{"another brand's newsletter", "26Q3 - CNCF - Newsletter - September", "OpenSSF", false},
		{"the brand's event email", "26Q3 - OpenSSF - Community Day - Registration Push", "OpenSSF", false},
		{"empty brand matches nothing", "26Q3 - OpenSSF - Newsletter", "  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBrandNewsletter(tc.email, tc.brand); got != tc.want {
				t.Fatalf("IsBrandNewsletter(%q, %q) = %v, want %v", tc.email, tc.brand, got, tc.want)
			}
		})
	}
}

func TestNewsletterBrandFromName(t *testing.T) {
	cases := []struct {
		name, want string
		ok         bool
	}{
		{"26Q2 - OpenSSF - Newsletter - October", "OpenSSF", true},
		{"26Q3 - CNCF - Newsletter Pytorch emailer", "CNCF", true},
		{"26Q3 PyTorch Foundation Newsletter Subscriptions", "PyTorch Foundation", true},
		{"LF AI & Data - Newsletter - May", "LF AI & Data", true},
		{"CNCF: Newsletter - June", "CNCF", true},
		{"26Q2 - Newsletter - October", "", false},
		{"26Q2 - OpenSSF - Community Day - Registration Push", "", false},
		// Names seen on the LF portal, where the first parser read a month or a period as the brand.
		{"26Q2 - PyTorch Foundation - November Newsletter 2026", "PyTorch Foundation", true},
		{"26Q4 - CNCF - SlashData Reports Newsletter NA", "CNCF", true},
		{"26Q4 - LF Newsletter - November 2026", "LF", true},
		{"(Oct 2026) Valkey Community Newsletter", "Valkey Community", true},
		{"Zephyr Project - Q3 2026 Newsletter", "Zephyr Project", true},
		{"ELISA Q3 2026 Newsletter", "ELISA", true},
		{"✉️ Dronecode Foundation Newsletter - October 2026", "Dronecode Foundation", true},
		{"OpenSearch Software Foundation: October Analyst Newsletter (Clone)", "OpenSearch Software Foundation", true},
		{"26Q3 - September 2026 Presto Community Newsletter", "Presto Community", true},
		{"2026 OCT - JDF Newsletter", "JDF", true},
		{"26Q3HPSF Q3 Community Newsletter", "HPSF Community", true},
		{"Q3 2026 - OCUDU newsletter Q3 2026 Edition", "OCUDU", true},
		{"26Q4 LF Education Newsletter - Oct 2026 -LC", "LF Education", true},
		{"26Q3 AGL Quarterly Newsletter - October 2026", "AGL", true},
		// Names that only MENTION a newsletter are no newsletter.
		{"Explore the Tracks at PyTorchCon Europe, 7-8 in Paris (LF Newsletter)", "", false},
		{"KC + CNC India | Schedule Live (LF Newsletter)", "", false},
		{"Context Engineering for Self-Healing AI SRE Newsletter", "", false},
		{"OWF_ Newsletter - May", "OWF", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NewsletterBrandFromName(tc.name)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NewsletterBrandFromName(%q) = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNewsletterSourceURL(t *testing.T) {
	blocks := []hubspot.EmailHTMLBlock{
		{HTML: `<a href="https://www.facebook.com/TheLinuxFoundation">fb</a> <a href="https://training.linuxfoundation.org/">training</a>`},
		{HTML: `<p>Explore <a href="https://www.linuxfoundation.org/blog/linux-foundation-newsletter-november-2026">this month</a></p>`},
		{HTML: `<a href="https://email.linuxfoundation.org/view">view</a> <a href="mailto:info@example.org">mail</a>`},
	}
	if got := newsletterSourceURL(blocks); got != "https://www.linuxfoundation.org/blog" {
		t.Fatalf("newsletterSourceURL = %q, want the blog index the issue links to", got)
	}

	noBlog := []hubspot.EmailHTMLBlock{
		{HTML: `<a href="https://openssf.org/events/">e</a> <a href="https://openssf.org/news/">n</a> <a href="https://x.com/openssf">x</a>`},
	}
	if got := newsletterSourceURL(noBlog); got != "https://openssf.org" {
		t.Fatalf("newsletterSourceURL = %q, want the site the issue links to most", got)
	}
	if got := newsletterSourceURL(nil); got != "" {
		t.Fatalf("newsletterSourceURL(nil) = %q, want empty", got)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// config_snapshot is persisted UNENCRYPTED and indexed (issue #245). These tests build each
// adapter's snapshot through the same campaignFrom* mapper Dispatch uses and pin two halves:
// every caller free-text and URL field is REDACTED in the stored copy, and the config that is
// sent to the platform still carries the FULL value.
//
// Each field gets its own marker, so dropping the sanitize call for any one field fails the
// test naming that field. The secret-shaped parts are a password-reset link's PATH and its
// QUERY — the realistic shape pkg/redact's SnapshotURL doc names — and a scheme-less copy of
// the same link, which only the path-only pass catches.

// snapLink is a password-reset link whose path and query each carry a per-field marker.
func snapLink(marker string) string {
	return "https://events.example/reset/" + marker + "PATH?reset=" + marker + "QUERY#" + marker + "FRAG"
}

// snapText is ad copy with a scheme-ful and a scheme-less reset link pasted into it.
func snapText(marker string) string {
	return "Register now " + snapLink(marker) + " or visit events.example/reset/" + marker + "BARE today"
}

// snapLeaks are the substrings of a marker's link that must never reach the snapshot.
func snapLeaks(marker string) []string {
	return []string{marker + "PATH", marker + "QUERY", marker + "FRAG", marker + "BARE"}
}

// assertSnapshotRedacted fails for every marker any of whose secret parts survived into the
// snapshot, and requires the sanitized host to be there so an emptied field cannot pass.
func assertSnapshotRedacted(t *testing.T, snapshot []byte, markers ...string) {
	t.Helper()
	if len(snapshot) == 0 {
		t.Fatal("ConfigSnapshot is empty; nothing was persisted to assert on")
	}
	s := string(snapshot)
	for _, m := range markers {
		for _, leak := range snapLeaks(m) {
			if strings.Contains(s, leak) {
				t.Errorf("snapshot keeps %q (field marker %s) — the free text/URL was not sanitized:\n%s", leak, m, s)
			}
		}
	}
	if !strings.Contains(s, "https://events.example") {
		t.Errorf("snapshot lost the sanitized scheme+host entirely:\n%s", s)
	}
}

// assertSentWhole fails unless the value the platform receives still has its full link.
func assertSentWhole(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s sent to the platform was changed by the snapshot: got %q, want %q", field, got, want)
	}
}

func TestLinkedInSnapshot_RedactsFreeTextAndKeepsTheSentConfigWhole(t *testing.T) {
	cfg := linkedinConfig{
		BudgetUSD:        100,
		TargetingProfile: snapText("LITP"),
		Variants: []linkedin.CreativeVariant{
			{IntroText: snapText("LIINTRO"), Headline: snapText("LIHEAD"), ImageURN: "urn:li:image:C4E22AQ"},
		},
		GeoTargets: []linkedin.GeoTarget{{Label: snapText("LIGEO"), URN: "urn:li:geo:103644278"}},
		TargetingProfiles: []linkedin.TargetingProfileConfig{
			{ID: snapText("LIPID"), Label: snapText("LIPLABEL"), Skills: []string{"urn:li:skill:1"}},
			// Not the selected profile, so the client never runs validFacets on its facets.
			{ID: "unselected", Skills: []string{snapText("LISKILL")}, Groups: []string{snapText("LIGROUP")}},
		},
	}
	c := campaignFromLinkedIn(context.Background(), &linkedin.CampaignResult{CampaignID: "c1", CreativeCount: 1}, 1, cfg)

	assertSnapshotRedacted(t, c.ConfigSnapshot, "LITP", "LIINTRO", "LIHEAD", "LIGEO", "LIPID", "LIPLABEL", "LISKILL", "LIGROUP")
	s := string(c.ConfigSnapshot)
	for _, keep := range []string{"Register now https://events.example or visit events.example today", "urn:li:image:C4E22AQ", "urn:li:geo:103644278", "urn:li:skill:1"} {
		if !strings.Contains(s, keep) {
			t.Errorf("snapshot should keep %q verbatim:\n%s", keep, s)
		}
	}
	assertSentWhole(t, "TargetingProfile", cfg.TargetingProfile, snapText("LITP"))
	assertSentWhole(t, "Variants[0].IntroText", cfg.Variants[0].IntroText, snapText("LIINTRO"))
	assertSentWhole(t, "Variants[0].Headline", cfg.Variants[0].Headline, snapText("LIHEAD"))
	assertSentWhole(t, "GeoTargets[0].Label", cfg.GeoTargets[0].Label, snapText("LIGEO"))
	assertSentWhole(t, "TargetingProfiles[0].ID", cfg.TargetingProfiles[0].ID, snapText("LIPID"))
	assertSentWhole(t, "TargetingProfiles[0].Label", cfg.TargetingProfiles[0].Label, snapText("LIPLABEL"))
	assertSentWhole(t, "TargetingProfiles[1].Skills[0]", cfg.TargetingProfiles[1].Skills[0], snapText("LISKILL"))
	assertSentWhole(t, "TargetingProfiles[1].Groups[0]", cfg.TargetingProfiles[1].Groups[0], snapText("LIGROUP"))
}

func TestMetaSnapshot_RedactsFreeTextAndURLsAndKeepsTheSentConfigWhole(t *testing.T) {
	cfg := metaConfig{
		Budget:          50,
		Objective:       "traffic",
		GeoTargets:      []string{"US", snapText("MGEO")},
		PixelID:         snapText("MPIXEL"),
		InstagramUserID: "17841400000000000",
		DSABeneficiary:  snapText("MDSAB"),
		DSAPayor:        snapText("MDSAP"),
		Variants: []meta.AdVariant{{
			PrimaryText: snapText("MPRIMARY"),
			Headline:    snapText("MHEAD"),
			Description: snapText("MDESC"),
			ImageURL:    snapLink("MIMG"),
		}},
	}
	c := campaignFromMeta(context.Background(), &meta.CampaignResult{CampaignID: "c1"}, cfg)

	assertSnapshotRedacted(t, c.ConfigSnapshot, "MGEO", "MPIXEL", "MDSAB", "MDSAP", "MPRIMARY", "MHEAD", "MDESC", "MIMG")
	s := string(c.ConfigSnapshot)
	for _, keep := range []string{"Register now https://events.example or visit events.example today", `"US"`, "17841400000000000", `"traffic"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("snapshot should keep %q verbatim:\n%s", keep, s)
		}
	}
	assertSentWhole(t, "GeoTargets[1]", cfg.GeoTargets[1], snapText("MGEO"))
	assertSentWhole(t, "PixelID", cfg.PixelID, snapText("MPIXEL"))
	assertSentWhole(t, "DSABeneficiary", cfg.DSABeneficiary, snapText("MDSAB"))
	assertSentWhole(t, "DSAPayor", cfg.DSAPayor, snapText("MDSAP"))
	assertSentWhole(t, "Variants[0].PrimaryText", cfg.Variants[0].PrimaryText, snapText("MPRIMARY"))
	assertSentWhole(t, "Variants[0].Headline", cfg.Variants[0].Headline, snapText("MHEAD"))
	assertSentWhole(t, "Variants[0].Description", cfg.Variants[0].Description, snapText("MDESC"))
	assertSentWhole(t, "Variants[0].ImageURL", cfg.Variants[0].ImageURL, snapLink("MIMG"))
}

func TestRedditSnapshot_RedactsFreeTextAndURLsAndKeepsTheSentConfigWhole(t *testing.T) {
	cfg := redditConfig{
		BudgetUSD:         75,
		Objective:         "traffic",
		GeoTargets:        []string{"US", snapText("RGEO")},
		Subreddits:        []string{"kubernetes", snapText("RSUB")},
		Interests:         []string{snapText("RINT")},
		Keywords:          []string{"cloud native", snapText("RKW")},
		Variants:          []reddit.AdVariant{{Headline: snapText("RHEAD"), Body: snapText("RBODY")}},
		PostURL:           snapLink("RPOST"),
		ImageURL:          snapLink("RIMG"),
		CallToAction:      snapText("RCTA"),
		ConversionPixelID: snapText("RPIXEL"),
		VideoGoal:         snapText("RVIDEO"),
	}
	c := campaignFromReddit(context.Background(), &reddit.CampaignResult{CampaignID: "c1"}, cfg)

	assertSnapshotRedacted(t, c.ConfigSnapshot, "RGEO", "RSUB", "RINT", "RKW", "RHEAD", "RBODY", "RPOST", "RIMG", "RCTA", "RPIXEL", "RVIDEO")
	s := string(c.ConfigSnapshot)
	for _, keep := range []string{"Register now https://events.example or visit events.example today", `"US"`, `"kubernetes"`, `"cloud native"`, `"traffic"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("snapshot should keep %q verbatim:\n%s", keep, s)
		}
	}
	assertSentWhole(t, "GeoTargets[1]", cfg.GeoTargets[1], snapText("RGEO"))
	assertSentWhole(t, "Subreddits[1]", cfg.Subreddits[1], snapText("RSUB"))
	assertSentWhole(t, "Interests[0]", cfg.Interests[0], snapText("RINT"))
	assertSentWhole(t, "Keywords[1]", cfg.Keywords[1], snapText("RKW"))
	assertSentWhole(t, "Variants[0].Headline", cfg.Variants[0].Headline, snapText("RHEAD"))
	assertSentWhole(t, "Variants[0].Body", cfg.Variants[0].Body, snapText("RBODY"))
	assertSentWhole(t, "PostURL", cfg.PostURL, snapLink("RPOST"))
	assertSentWhole(t, "ImageURL", cfg.ImageURL, snapLink("RIMG"))
	assertSentWhole(t, "CallToAction", cfg.CallToAction, snapText("RCTA"))
}

// A nil slice stays null and an empty one stays [] in the snapshot, so the stored record says
// what the caller sent rather than what the redactor allocated.
func TestSanitizeSnapshotTexts_PreservesNilAndEmpty(t *testing.T) {
	if got := sanitizeSnapshotTexts(nil); got != nil {
		t.Errorf("nil input: got %#v, want nil", got)
	}
	if got := sanitizeSnapshotTexts([]string{}); got == nil || len(got) != 0 {
		t.Errorf("empty input: got %#v, want a non-nil empty slice", got)
	}
	in := []string{snapText("TXT")}
	out := sanitizeSnapshotTexts(in)
	if in[0] != snapText("TXT") {
		t.Errorf("input was written through: %q", in[0])
	}
	if strings.Contains(out[0], "TXTPATH") || strings.Contains(out[0], "TXTBARE") {
		t.Errorf("output not sanitized: %q", out[0])
	}
}

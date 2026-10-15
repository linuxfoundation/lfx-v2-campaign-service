// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strconv"
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

// The values every sanitized field must hold in the snapshot: snapText with both links reduced
// to their host, and snapLink reduced to its scheme+host. Each field is checked against these
// by value after decoding, so a field the builder drops or empties fails by name — a
// whole-snapshot substring check would pass on any one surviving copy.
const (
	snapTextSanitized = "Register now https://events.example or visit events.example today"
	snapLinkSanitized = "https://events.example"
)

// decodeSnapshot unmarshals the persisted ConfigSnapshot back into the adapter's config type.
func decodeSnapshot[T any](t *testing.T, snapshot []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(snapshot, &out); err != nil {
		t.Fatalf("ConfigSnapshot does not decode: %v\n%s", err, snapshot)
	}
	return out
}

// assertStored fails unless the decoded snapshot field holds exactly want.
func assertStored(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("snapshot %s: got %q, want %q", field, got, want)
	}
}

// assertStoredList fails unless the decoded snapshot list holds exactly want, element by element.
func assertStoredList(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("snapshot %s: got %q, want %q", field, got, want)
		return
	}
	for i := range want {
		assertStored(t, field+"["+strconv.Itoa(i)+"]", got[i], want[i])
	}
}

// assertSnapshotRedacted fails for every marker any of whose secret parts survived anywhere in
// the snapshot. It is the leak half only; each field's stored value is checked by assertStored.
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
	got := decodeSnapshot[linkedinConfig](t, c.ConfigSnapshot)
	assertStored(t, "TargetingProfile", got.TargetingProfile, snapTextSanitized)
	if len(got.Variants) != 1 || len(got.GeoTargets) != 1 || len(got.TargetingProfiles) != 2 {
		t.Fatalf("snapshot lost list entries: %d variants, %d geo targets, %d profiles", len(got.Variants), len(got.GeoTargets), len(got.TargetingProfiles))
	}
	assertStored(t, "Variants[0].IntroText", got.Variants[0].IntroText, snapTextSanitized)
	assertStored(t, "Variants[0].Headline", got.Variants[0].Headline, snapTextSanitized)
	assertStored(t, "Variants[0].ImageURN", got.Variants[0].ImageURN, "urn:li:image:C4E22AQ")
	assertStored(t, "GeoTargets[0].Label", got.GeoTargets[0].Label, snapTextSanitized)
	assertStored(t, "GeoTargets[0].URN", got.GeoTargets[0].URN, "urn:li:geo:103644278")
	assertStored(t, "TargetingProfiles[0].ID", got.TargetingProfiles[0].ID, snapTextSanitized)
	assertStored(t, "TargetingProfiles[0].Label", got.TargetingProfiles[0].Label, snapTextSanitized)
	assertStoredList(t, "TargetingProfiles[0].Skills", got.TargetingProfiles[0].Skills, []string{"urn:li:skill:1"})
	assertStored(t, "TargetingProfiles[1].ID", got.TargetingProfiles[1].ID, "unselected")
	assertStoredList(t, "TargetingProfiles[1].Skills", got.TargetingProfiles[1].Skills, []string{snapTextSanitized})
	assertStoredList(t, "TargetingProfiles[1].Groups", got.TargetingProfiles[1].Groups, []string{snapTextSanitized})
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
	got := decodeSnapshot[metaConfig](t, c.ConfigSnapshot)
	assertStored(t, "Objective", got.Objective, "traffic")
	assertStored(t, "InstagramUserID", got.InstagramUserID, "17841400000000000")
	assertStoredList(t, "GeoTargets", got.GeoTargets, []string{"US", snapTextSanitized})
	assertStored(t, "PixelID", got.PixelID, snapTextSanitized)
	assertStored(t, "DSABeneficiary", got.DSABeneficiary, snapTextSanitized)
	assertStored(t, "DSAPayor", got.DSAPayor, snapTextSanitized)
	if len(got.Variants) != 1 {
		t.Fatalf("snapshot has %d variants, want 1", len(got.Variants))
	}
	assertStored(t, "Variants[0].PrimaryText", got.Variants[0].PrimaryText, snapTextSanitized)
	assertStored(t, "Variants[0].Headline", got.Variants[0].Headline, snapTextSanitized)
	assertStored(t, "Variants[0].Description", got.Variants[0].Description, snapTextSanitized)
	assertStored(t, "Variants[0].ImageURL", got.Variants[0].ImageURL, snapLinkSanitized)
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
	got := decodeSnapshot[redditConfig](t, c.ConfigSnapshot)
	assertStored(t, "Objective", got.Objective, "traffic")
	assertStoredList(t, "GeoTargets", got.GeoTargets, []string{"US", snapTextSanitized})
	assertStoredList(t, "Subreddits", got.Subreddits, []string{"kubernetes", snapTextSanitized})
	assertStoredList(t, "Interests", got.Interests, []string{snapTextSanitized})
	assertStoredList(t, "Keywords", got.Keywords, []string{"cloud native", snapTextSanitized})
	if len(got.Variants) != 1 {
		t.Fatalf("snapshot has %d variants, want 1", len(got.Variants))
	}
	assertStored(t, "Variants[0].Headline", got.Variants[0].Headline, snapTextSanitized)
	assertStored(t, "Variants[0].Body", got.Variants[0].Body, snapTextSanitized)
	assertStored(t, "PostURL", got.PostURL, snapLinkSanitized)
	assertStored(t, "ImageURL", got.ImageURL, snapLinkSanitized)
	assertStored(t, "CallToAction", got.CallToAction, snapTextSanitized)
	assertStored(t, "ConversionPixelID", got.ConversionPixelID, snapTextSanitized)
	assertStored(t, "VideoGoal", got.VideoGoal, snapTextSanitized)
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

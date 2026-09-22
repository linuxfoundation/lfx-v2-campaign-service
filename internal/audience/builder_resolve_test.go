// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kubeConTerms is the event most of these cases classify against: a real LF event name,
// with the "+" and the regional suffix that made the old contiguous-phrase search fail.
var kubeConTerms = NewLastSentTerms("KubeCon + CloudNativeCon North America 2026", "CNCF")

// TestMatchLastSent_AYearStrippedEventNameIsNotOneContiguousTerm pins the regression that
// emptied this endpoint. The event name was searched as ONE substring, so
// "KubeCon + CloudNativeCon North America" had to appear verbatim in an email's name or
// subject for it to be found — and no marketing email is ever named that. The sweep
// returned zero candidates for an event with a full history of sends, and the operator read
// the empty panel as "this event has never been emailed".
func TestMatchLastSent_AYearStrippedEventNameIsNotOneContiguousTerm(t *testing.T) {
	m := MatchLastSent("KubeCon NA 2026 — Registration Open", "", kubeConTerms)

	assert.True(t, m.Matched,
		"an email sharing the event's distinctive token was not found, because the whole event name was matched as one phrase")
	assert.False(t, m.BrandOnly, "the EVENT matched, so this is not a brand fallback")
	assert.Equal(t, 1, m.Overlap, `only "kubecon" overlaps: "na" is dropped as ≤2 chars and "2026" as a year`)
}

// TestMatchLastSent_ScoresSubjectAsWellAsName pins that either field can carry the event.
// The search matched name OR subject while the ranking scored the name ALONE, so a
// subject-only match scored zero and was truncated away behind emails that merely had
// wordier names. An email named "Registration Open" is the same send whichever field the
// event landed in.
func TestMatchLastSent_ScoresSubjectAsWellAsName(t *testing.T) {
	subjectOnly := MatchLastSent("Registration Open", "KubeCon closes Friday", kubeConTerms)
	require.True(t, subjectOnly.Matched, "the event is in the subject and must still count")
	assert.Positive(t, subjectOnly.Overlap,
		"a subject-only match scored zero and lost its place to any email with a wordier name")

	nameOnly := MatchLastSent("KubeCon closes Friday", "Registration Open", kubeConTerms)
	assert.Equal(t, nameOnly.Overlap, subjectOnly.Overlap,
		"the same words in the two fields swapped must rank identically; neither field is privileged")
}

// TestMatchLastSent_AGenericWordAloneIsNotAMatch is the false-positive guard on the
// tokenized rule. Tokenizing is what makes the endpoint find anything at all, but it also
// makes a single common word enough to match — and "summit" is shared by a dozen unrelated
// portfolio sends. A wrong row here is worse than a missing one: the operator builds the
// next audience from what this panel calls precedent.
func TestMatchLastSent_AGenericWordAloneIsNotAMatch(t *testing.T) {
	oss := NewLastSentTerms("Open Source Summit Europe 2026", "LinuxFoundation")

	recap := MatchLastSent("Summit Recap", "", oss)
	assert.False(t, recap.Matched, `"summit" alone is not evidence that an email belongs to this event`)
	assert.Equal(t, 1, recap.Overlap,
		"a generic token still RANKS — it is only barred from admitting a match by itself")

	assert.True(t, MatchLastSent("Open Source Summit EU", "", oss).Matched,
		"two distinctive tokens are an unambiguous match")
	assert.True(t, MatchLastSent("Summit Europe agenda", "", oss).Matched,
		"two generic tokens together are specific enough, which is why Overlap>=2 also admits")
}

// TestMatchLastSent_ABrandOnlyHitIsFlaggedAsFallback pins that the brand is reported AS a
// fallback rather than as a match. brand_short is optional and deliberately broad — it
// matches every send in the portfolio — so the caller needs to know a row arrived on the
// brand alone in order to drop it when the event itself matched elsewhere.
func TestMatchLastSent_ABrandOnlyHitIsFlaggedAsFallback(t *testing.T) {
	m := MatchLastSent("CNCF Newsletter", "Monthly roundup", kubeConTerms)

	require.True(t, m.Matched, "the brand is a real last resort for a renamed or first-time event")
	assert.True(t, m.BrandOnly,
		"an unflagged brand hit is indistinguishable from an event match and cannot be demoted")
	assert.Zero(t, m.Overlap, "nothing in the event name matched, so there is nothing to rank on")
}

// TestNewLastSentTerms_DropsTheYearAndTheStopwords pins what the tiers are built from. The
// year has to go: "KubeCon Europe 2026" would otherwise find only THIS year's emails, and
// this year's is precisely the edition that has not been sent yet.
func TestNewLastSentTerms_DropsTheYearAndTheStopwords(t *testing.T) {
	terms := NewLastSentTerms("KubeCon Europe 2026", "CNCF")

	assert.Equal(t, map[string]struct{}{"kubecon": {}}, terms.Event,
		"only the distinctive token belongs to the tier that can admit a match alone")
	assert.Equal(t, map[string]struct{}{"europe": {}}, terms.Generic,
		"a region recurs across the portfolio and must rank without admitting")
	assert.Equal(t, map[string]struct{}{"cncf": {}}, terms.Brand)
	assert.False(t, MatchLastSent("KubeCon Europe 2026", "", terms).Overlap > 2,
		"the year must not be a term of its own, or only the unsent edition would match")

	// A brand token that is ALSO an event token is the event match, not a second tier of
	// evidence — otherwise the fallback claims credit for a hit the event name explains,
	// and every event-matching row would be demoted as brand-only.
	shared := NewLastSentTerms("CNCF Project Update", "CNCF")
	assert.NotContains(t, shared.Brand, "cncf")
	assert.False(t, MatchLastSent("CNCF Project Update", "", shared).BrandOnly)
}

// TestNewLastSentTerms_AnEmptyEventNameMatchesNothing pins that no evidence is not a match.
// An event with no name and no brand yields empty tiers, and an empty term set must reject
// every candidate rather than accept every one — the same rule the resolvers above hold to.
func TestNewLastSentTerms_AnEmptyEventNameMatchesNothing(t *testing.T) {
	terms := NewLastSentTerms("", "")

	assert.Empty(t, terms.Event)
	assert.Empty(t, terms.Generic)
	assert.Empty(t, terms.Brand)
	assert.False(t, MatchLastSent("KubeCon NA 2026", "Registration Open", terms).Matched,
		"empty terms accepted an arbitrary email, which would present an unrelated send as precedent")
}

// TestStripYear_RemovesTheEditionAndClosesTheGap pins the helper both tiers are built on.
// It is not only a delete: leaving the double space behind would tokenize the same but
// reads as a different name everywhere the term is logged or displayed.
func TestStripYear_RemovesTheEditionAndClosesTheGap(t *testing.T) {
	assert.Equal(t, "KubeCon Europe", StripYear("KubeCon Europe 2026"))
	assert.Equal(t, "KubeCon Europe", StripYear("KubeCon 2026 Europe"))
	assert.Equal(t, "KubeCon Europe", StripYear("KubeCon Europe"), "a name with no year is unchanged")
	assert.Equal(t, "Room 404 Sessions", StripYear("Room 404 Sessions"),
		"only a four-digit 19xx/20xx year is an edition; an arbitrary number is part of the name")
}

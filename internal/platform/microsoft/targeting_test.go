// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// validInputWithKeywords is validInput plus three DISTINCTIVE keywords. Every field differs
// from every other — three different texts AND three different match types — so a swap
// between two keywords, or between a text and a match type, changes the asserted body. A
// fixture with a repeated match type would stay green through exactly that bug.
func validInputWithKeywords() CampaignInput {
	in := validInput()
	in.Keywords = []Keyword{
		{Text: "kubernetes training", MatchType: MatchTypeExact},
		{Text: "cloud native conference", MatchType: MatchTypePhrase},
		{Text: "open source summit", MatchType: MatchTypeBroad},
	}
	return in
}

// ---- the request body -------------------------------------------------------

// TestCreateCampaign_KeywordBodyPinsEveryFieldToItsOwnSlot is the anti-swap test: it asserts
// each keyword's text and match type land TOGETHER in the right entry, so a client that
// paired keyword[0]'s text with keyword[1]'s match type fails here rather than shipping a
// campaign that matches the wrong queries.
func TestCreateCampaign_KeywordBodyPinsEveryFieldToItsOwnSlot(t *testing.T) {
	var kw createKeywordsRequest
	api := &campaignsAPI{keywordSeen: &kw}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInputWithKeywords())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// AdGroupId is a SIBLING of the Keywords array (AddKeywords carries the parent at the top
	// level), and it must be the ad group this run created — not the campaign id, which is the
	// obvious wrong-parent bug.
	if got := kw.AdGroupId.String(); got != "654" {
		t.Errorf("AddKeywords AdGroupId = %q, want the ad group id 654 (not the campaign id)", got)
	}
	if len(kw.Keywords) != 3 {
		t.Fatalf("sent %d keywords, want 3: %+v", len(kw.Keywords), kw.Keywords)
	}
	// Each (text, matchType) PAIR is asserted together, in order.
	want := []struct{ text, match string }{
		{"kubernetes training", MatchTypeExact},
		{"cloud native conference", MatchTypePhrase},
		{"open source summit", MatchTypeBroad},
	}
	for i, w := range want {
		if kw.Keywords[i].Text != w.text {
			t.Errorf("keyword[%d].Text = %q, want %q", i, kw.Keywords[i].Text, w.text)
		}
		if kw.Keywords[i].MatchType != w.match {
			t.Errorf("keyword[%d].MatchType = %q, want %q (paired with text %q)", i, kw.Keywords[i].MatchType, w.match, w.text)
		}
		// Every keyword is created PAUSED so an operator reviews the list before it spends.
		if kw.Keywords[i].Status != keywordStatusPaused {
			t.Errorf("keyword[%d].Status = %q, want %q", i, kw.Keywords[i].Status, keywordStatusPaused)
		}
		// No per-keyword bid: keywords inherit the AD GROUP's CpcBid, so one bid lives in one place.
		if kw.Keywords[i].Bid != nil {
			t.Errorf("keyword[%d] carries a per-keyword Bid %+v; keywords must inherit the ad group CpcBid", i, kw.Keywords[i].Bid)
		}
	}
	// The parsed ids are surfaced for the toggle guard to read.
	if len(res.KeywordIDs) != 3 || res.KeywordIDs[0] != "701" || res.KeywordIDs[2] != "703" {
		t.Errorf("KeywordIDs = %v, want [701 702 703]", res.KeywordIDs)
	}
}

// TestCreateCampaign_KeywordsGoToTheirOwnEndpoint pins the ROUTE. Keywords are not
// AdGroupCriterions: the AdGroupCriterionType enum has no Keyword member, so a create routed
// there is rejected. This asserts the client posts to /Keywords and never to
// /AdGroupCriterions.
func TestCreateCampaign_KeywordsGoToTheirOwnEndpoint(t *testing.T) {
	var paths []string
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/AdGroupCriterions") {
			t.Errorf("keywords must NOT be sent to /AdGroupCriterions (its criterion-type enum has no Keyword member)")
		}
		base(w, r)
	})
	if _, err := c.CreateCampaign(context.Background(), validInputWithKeywords()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	var sawKeywords bool
	for _, p := range paths {
		if strings.HasSuffix(p, "/Keywords") {
			sawKeywords = true
		}
	}
	if !sawKeywords {
		t.Errorf("no POST to /Keywords was issued; paths = %v", paths)
	}
}

// TestCreateCampaign_KeywordStepRunsLast pins the ORDER. Keywords are attached only after the
// ad group and ad exist: keywording an ad group whose ad might still fail would leave paid
// criteria on an incomplete tree.
func TestCreateCampaign_KeywordStepRunsLast(t *testing.T) {
	var creates []string
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// Record CREATES only — the QueryBy… reads are idempotency lookups, not steps.
		if !strings.Contains(p, "QueryBy") {
			creates = append(creates, p[strings.LastIndex(p, "/")+1:])
		}
		base(w, r)
	})
	if _, err := c.CreateCampaign(context.Background(), validInputWithKeywords()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	want := []string{"Campaigns", "AdGroups", "Ads", "Keywords"}
	if len(creates) != len(want) {
		t.Fatalf("create order = %v, want %v", creates, want)
	}
	for i := range want {
		if creates[i] != want[i] {
			t.Fatalf("create order = %v, want %v", creates, want)
		}
	}
}

// ---- the ad-group bid -------------------------------------------------------

// TestCreateCampaign_CpcBidIsSentWhenSupplied uses a DISTINCTIVE, non-round amount so the
// value cannot be confused with the budget (50) or with any default.
func TestCreateCampaign_CpcBidIsSentWhenSupplied(t *testing.T) {
	var group createAdGroupsRequest
	api := &campaignsAPI{adGroupSeen: &group}
	c := newAPIClient(t, api.handler(t))
	in := validInputWithKeywords()
	in.CpcBid = 3.47

	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	got := group.AdGroups[0].CpcBid
	if got == nil {
		t.Fatal("ad group CpcBid was omitted despite a supplied bid")
	}
	if got.Amount != 3.47 {
		// Guards against the bid being crossed with the budget: validInput's Budget is 50.
		t.Errorf("ad group CpcBid.Amount = %v, want 3.47 (the bid, not the budget)", got.Amount)
	}
}

// TestCreateCampaign_UnsetCpcBidOmitsTheField pins the distinction between "no bid" and "a
// bid of zero". Microsoft floors an OMITTED bid to the account-currency minimum, which is
// serve-capable; an explicit {"Amount":0} is a zero bid, which is not the same thing. A
// float64 field without a pointer would serialize the unset case as the latter, so this
// asserts against the RAW body rather than the decoded struct — a decoded zero cannot tell
// the two apart, which is exactly the bug being guarded.
func TestCreateCampaign_UnsetCpcBidOmitsTheField(t *testing.T) {
	var rawBody string
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
			return
		}
		base(w, r)
	})
	in := validInputWithKeywords() // CpcBid left at its zero value = unset
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if strings.Contains(rawBody, "CpcBid") {
		t.Errorf("an unset CpcBid must be OMITTED, not sent as an explicit zero bid: %s", rawBody)
	}
}

// ---- validation (all up front, before any create) ---------------------------

// TestCreateCampaign_RejectsBadKeywordsBeforeAnyCreate is the orphan guard: an invalid
// keyword must fail BEFORE the campaign is created, because keywords are attached at the LAST
// step and a late failure would leave a paid PAUSED campaign, ad group and ad behind.
func TestCreateCampaign_RejectsBadKeywordsBeforeAnyCreate(t *testing.T) {
	longText := strings.Repeat("k", maxKeywordTextRunes+1)
	cases := []struct {
		name    string
		kw      []Keyword
		wantErr string
	}{
		{"empty text", []Keyword{{Text: "   ", MatchType: MatchTypeExact}}, "must not be empty"},
		{"over-long text", []Keyword{{Text: longText, MatchType: MatchTypeExact}}, "exceeding the"},
		{"bad match type", []Keyword{{Text: "kubernetes", MatchType: "REGEX"}}, "unsupported match type"},
		{"missing match type", []Keyword{{Text: "kubernetes", MatchType: ""}}, "unsupported match type"},
		{"control character", []Keyword{{Text: "kube\tnetes", MatchType: MatchTypeExact}}, "control character"},
		{"too many", make([]Keyword, maxKeywords+1), "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			})
			in := validInput()
			in.Keywords = tc.kw
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("expected a validation error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			// (nil, err) — nothing was created, so there is nothing to reconcile.
			if res != nil {
				t.Errorf("a pre-create validation failure must return a nil result, got %+v", res)
			}
			if reached {
				t.Error("a bad keyword must be rejected BEFORE any upstream request")
			}
		})
	}
}

// TestCreateCampaign_RejectsBadCpcBidBeforeAnyCreate — same orphan guard for the bid.
func TestCreateCampaign_RejectsBadCpcBidBeforeAnyCreate(t *testing.T) {
	for _, tc := range []struct {
		name string
		bid  float64
	}{
		{"below minimum", minCpcBid / 2},
		{"above maximum", maxCpcBid + 1},
		{"negative", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			})
			in := validInput()
			in.CpcBid = tc.bid
			if _, err := c.CreateCampaign(context.Background(), in); err == nil {
				t.Fatalf("expected a validation error for a %s bid", tc.name)
			}
			if reached {
				t.Error("a bad bid must be rejected BEFORE any upstream request")
			}
		})
	}
}

// TestValidateKeywords_DedupesCaseInsensitivelyKeepingOriginalCasing pins both halves: the
// DUPLICATE is dropped (Microsoft rejects the whole create otherwise) while the SENT text
// keeps the caller's casing (the dedupe key is folded, the value is not).
func TestValidateKeywords_DedupesCaseInsensitivelyKeepingOriginalCasing(t *testing.T) {
	got, err := validateKeywords([]Keyword{
		{Text: "Kubernetes", MatchType: MatchTypeExact},
		{Text: "kubernetes", MatchType: MatchTypeExact},
		// SAME text, DIFFERENT match type — a distinct criterion, so it must survive.
		{Text: "Kubernetes", MatchType: MatchTypePhrase},
	})
	if err != nil {
		t.Fatalf("validateKeywords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d keywords, want 2 (the case-variant duplicate dropped, the other match type kept): %+v", len(got), got)
	}
	if got[0].Text != "Kubernetes" {
		t.Errorf("kept text = %q, want the caller's original casing %q", got[0].Text, "Kubernetes")
	}
	if got[1].MatchType != MatchTypePhrase {
		t.Errorf("second entry match type = %q, want %q", got[1].MatchType, MatchTypePhrase)
	}
}

// TestValidateKeywords_AcceptsGoogleAdsCasing: a caller reusing a Google Ads payload sends
// SCREAMING_CASE match types. Those are canonicalized to Microsoft's PascalCase rather than
// refused over spelling, but the value SENT must be Microsoft's.
func TestValidateKeywords_AcceptsGoogleAdsCasing(t *testing.T) {
	got, err := validateKeywords([]Keyword{{Text: "kubernetes", MatchType: "EXACT"}})
	if err != nil {
		t.Fatalf("validateKeywords: %v", err)
	}
	if got[0].MatchType != MatchTypeExact {
		t.Errorf("match type = %q, want the Microsoft spelling %q", got[0].MatchType, MatchTypeExact)
	}
}

// TestValidateKeywords_EmptyInputIsNotAnError: keywords are optional at the client layer.
// Whether an un-keyworded campaign may be ACTIVATED is the dispatcher's decision.
func TestValidateKeywords_EmptyInputIsNotAnError(t *testing.T) {
	got, err := validateKeywords(nil)
	if err != nil || got != nil {
		t.Errorf("validateKeywords(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

// ---- failure classification -------------------------------------------------

// TestCreateCampaign_KeywordPartialErrorIsRejectedNotUnconfirmed: a definite per-entity
// rejection means nothing was created, so it must NOT be reported as "may exist" — that would
// send an operator hunting for keywords that are not there. The result still carries the
// campaign/ad-group/ad ids, which DO exist.
func TestCreateCampaign_KeywordPartialErrorIsRejectedNotUnconfirmed(t *testing.T) {
	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[null],"PartialErrors":[{"Code":1042,"ErrorCode":"CampaignServiceEditorialError"}]}`}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInputWithKeywords())
	if err == nil {
		t.Fatal("expected a keyword rejection")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("a definite PartialError must read as rejected, got: %v", err)
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a definite rejection must NOT be reported as UNCONFIRMED: %v", err)
	}
	// The tree above the keywords exists and must stay reconcilable.
	if res == nil || res.CampaignID != "321" || res.AdGroupID != "654" || res.AdID != "987" {
		t.Fatalf("expected a partial carrying the created tree, got %+v", res)
	}
	if len(res.KeywordIDs) != 0 {
		t.Errorf("no keywords were created, so KeywordIDs must be empty, got %v", res.KeywordIDs)
	}
}

// TestCreateCampaign_ShortKeywordIDArrayIsUnconfirmed: a 200 whose id array does not describe
// what was sent leaves it unknown which keywords exist upstream. Because AddKeywords has NO
// idempotency key, a blind retry would DUPLICATE them — so this must be UNCONFIRMED, never a
// clean failure that invites a retry.
func TestCreateCampaign_ShortKeywordIDArrayIsUnconfirmed(t *testing.T) {
	// Three keywords sent, two ids returned.
	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[701,702],"PartialErrors":[]}`}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInputWithKeywords())
	if err == nil {
		t.Fatal("expected an unconfirmed outcome for a short id array")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a short id array must be UNCONFIRMED, got: %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("the error should warn that a blind retry duplicates keywords, got: %v", err)
	}
	if res == nil || res.CampaignID != "321" {
		t.Fatalf("expected a partial carrying the created tree, got %+v", res)
	}
}

// TestCreateCampaign_NullKeywordIDIsUnconfirmed: a null id slot with NO PartialError to
// explain it is the malformed-200 case — the keyword may exist but cannot be identified.
func TestCreateCampaign_NullKeywordIDIsUnconfirmed(t *testing.T) {
	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[701,null,703],"PartialErrors":[]}`}
	c := newAPIClient(t, api.handler(t))

	_, err := c.CreateCampaign(context.Background(), validInputWithKeywords())
	if err == nil {
		t.Fatal("expected an unconfirmed outcome for a null id slot")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a null id slot with no PartialError must be UNCONFIRMED, got: %v", err)
	}
}

// TestCreateCampaign_KeywordCreateIsNotRetriedOn429 is the money guard. AddKeywords is a
// MUTATION with no idempotency key, so a 429 retry would add a SECOND copy of every keyword.
// The read lookups may retry; this create must not.
func TestCreateCampaign_KeywordCreateIsNotRetriedOn429(t *testing.T) {
	var keywordPosts int
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Keywords") {
			keywordPosts++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
			return
		}
		base(w, r)
	})
	if _, err := c.CreateCampaign(context.Background(), validInputWithKeywords()); err == nil {
		t.Fatal("expected the 429 to surface as an error")
	}
	if keywordPosts != 1 {
		t.Errorf("AddKeywords was sent %d times; a mutating create must NOT be retried on 429 (each retry duplicates every keyword)", keywordPosts)
	}
}

// ---- the no-keyword path ----------------------------------------------------

// TestCreateCampaign_NoKeywordsSkipsTheCallAndSaysSo: with no keywords supplied the client
// issues no keyword request at all (an empty create would be a pointless paid round-trip),
// and the Steps say plainly that the campaign cannot serve — an operator reading the result
// should not have to infer that from a missing line.
func TestCreateCampaign_NoKeywordsSkipsTheCallAndSaysSo(t *testing.T) {
	var keywordCalled bool
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Keywords") {
			keywordCalled = true
		}
		base(w, r)
	})
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if keywordCalled {
		t.Error("no keywords were supplied, so no /Keywords request should be sent")
	}
	if len(res.KeywordIDs) != 0 {
		t.Errorf("KeywordIDs = %v, want empty", res.KeywordIDs)
	}
	var said bool
	for _, s := range res.Steps {
		if strings.Contains(s, "cannot serve") {
			said = true
		}
	}
	if !said {
		t.Errorf("the steps must state that the campaign cannot serve without keywords, got %v", res.Steps)
	}
}

// ---- the status cascade -----------------------------------------------------

// TestUpdateCampaignAndChildrenStatus_ActivateEnablesKeywordsBeforeTheGate pins the reason
// keywords are in the cascade at all: they are CREATED Paused, so an activate that skipped
// them would enable the campaign, ad group and ad while every keyword stayed Paused — a
// campaign reporting Active that serves nothing.
func TestUpdateCampaignAndChildrenStatus_ActivateEnablesKeywordsBeforeTheGate(t *testing.T) {
	var paths []string
	var keywordBody updateKeywordsRequest
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		if strings.HasSuffix(r.URL.Path, "/Keywords") {
			_ = json.NewDecoder(r.Body).Decode(&keywordBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})

	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", []string{"701", "702"}, StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	want := []string{"AdGroups", "Ads", "Keywords", "Campaigns"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("activate order = %v, want %v (the campaign gate must flip LAST)", paths, want)
	}
	// Keywords are addressed THROUGH their ad group, and each id must carry the new status.
	if keywordBody.AdGroupId.String() != "654" {
		t.Errorf("keyword PUT AdGroupId = %q, want 654", keywordBody.AdGroupId.String())
	}
	if len(keywordBody.Keywords) != 2 {
		t.Fatalf("keyword PUT carried %d keywords, want 2", len(keywordBody.Keywords))
	}
	for i, k := range keywordBody.Keywords {
		if k.Status != StatusActive {
			t.Errorf("keyword[%d].Status = %q, want %q", i, k.Status, StatusActive)
		}
	}
	if keywordBody.Keywords[0].Id.String() != "701" || keywordBody.Keywords[1].Id.String() != "702" {
		t.Errorf("keyword ids = %q/%q, want 701/702", keywordBody.Keywords[0].Id.String(), keywordBody.Keywords[1].Id.String())
	}
}

// TestUpdateCampaignAndChildrenStatus_PauseGatesFirstThenKeywords: on PAUSE the campaign gate
// flips FIRST (delivery stops immediately even if a later call fails); keywords are last,
// leaving the tree in the same all-Paused shape a fresh create produces.
func TestUpdateCampaignAndChildrenStatus_PauseGatesFirstThenKeywords(t *testing.T) {
	var paths []string
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})

	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", []string{"701"}, StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	want := []string{"Campaigns", "AdGroups", "Ads", "Keywords"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("pause order = %v, want %v (the gate must flip FIRST)", paths, want)
	}
}

// TestUpdateCampaignAndChildrenStatus_RejectsBadKeywordIDsWithoutCalling: every keyword id is
// validated BEFORE any mutation. A bad id found mid-cascade would fail after the campaign and
// ad group had already flipped, turning a rejectable input error into a partial cascade.
func TestUpdateCampaignAndChildrenStatus_RejectsBadKeywordIDsWithoutCalling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		adGroupID  string
		keywordIDs []string
		wantErr    string
	}{
		{"non-numeric id", "654", []string{"701", "abc"}, "not a numeric id"},
		{"negative id", "654", []string{"-5"}, "not a numeric id"},
		// A keyword is addressed through its ad group, so ids with no ad group are unusable.
		{"keywords without an ad group", "", []string{"701"}, "no ad group id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
			})
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", tc.adGroupID, "", tc.keywordIDs, StatusPaused)
			if err == nil {
				t.Fatalf("expected a rejection for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if called {
				t.Error("a bad keyword id must be rejected WITHOUT calling Microsoft")
			}
		})
	}
}

// TestUpdateCampaignAndChildrenStatus_NoKeywordsSkipsTheKeywordPut: an empty id slice must
// not send an empty PUT (which would address no keywords and could be rejected outright).
func TestUpdateCampaignAndChildrenStatus_NoKeywordsSkipsTheKeywordPut(t *testing.T) {
	var keywordCalled bool
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Keywords") {
			keywordCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", nil, StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	if keywordCalled {
		t.Error("with no keyword ids, no /Keywords PUT should be sent")
	}
}

// Every keyword id must survive the decode, not just the first sixteen.
//
// KeywordIds originally decoded through boundedNumberIDs, whose retention limit
// (maxDecodedErrorItems = 16) is sized for a campaign create — one id — and for error arrays.
// AddKeywords sends up to maxKeywords (60), and every id is what the status cascade enables on
// ACTIVATE. A 60-keyword response therefore decoded short BY DESIGN, the cardinality check
// lowered its own expectation to match, and 44 keywords stayed Paused on a campaign the operator
// believes is fully live.
//
// 20 is deliberately just past the old bound: enough to fail against it, small enough to read.
func TestCreateCampaign_RetainsEveryKeywordIDPastTheErrorArrayBound(t *testing.T) {
	const n = 20
	in := validInput()
	in.Keywords = make([]Keyword, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		in.Keywords = append(in.Keywords, Keyword{Text: fmt.Sprintf("keyword %02d", i), MatchType: MatchTypeExact})
		ids = append(ids, strconv.Itoa(700+i))
	}

	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[` + strings.Join(ids, ",") + `],"PartialErrors":[]}`}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(res.KeywordIDs) != n {
		t.Fatalf("KeywordIDs kept %d of %d — ids past the decode bound were dropped, so ACTIVATE would enable only those kept", len(res.KeywordIDs), n)
	}
	// Assert the VALUES, not just the count: a bound that silently returned the right number of
	// wrong ids would pass a length check.
	for i, want := range ids {
		if res.KeywordIDs[i] != want {
			t.Errorf("KeywordIDs[%d] = %q, want %q", i, res.KeywordIDs[i], want)
		}
	}
}

// A response genuinely SHORTER than the request is now a real defect rather than an expected
// consequence of the decode bound, so it must be refused rather than silently accepted.
func TestCreateCampaign_ShortKeywordResponseIsRefused(t *testing.T) {
	in := validInput()
	in.Keywords = []Keyword{
		{Text: "one", MatchType: MatchTypeExact},
		{Text: "two", MatchType: MatchTypeExact},
		{Text: "three", MatchType: MatchTypeExact},
	}
	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[701,702],"PartialErrors":[]}`}
	c := newAPIClient(t, api.handler(t))

	if _, err := c.CreateCampaign(context.Background(), in); err == nil {
		t.Fatal("a response carrying 2 ids for 3 keywords was accepted; the missing keyword would stay Paused")
	}
}

// AddKeywords is a BATCH: a rejection of one keyword sits alongside real ids for the ones that
// succeeded. `[701, null, 703]` with a PartialError means TWO keywords exist upstream, not none.
//
// Those ids must survive the error. They are what the status cascade enables on ACTIVATE, and
// what stops a reconciliation creating a second copy — a blind retry of the batch would
// duplicate every keyword that did succeed. Reporting "rejected" with an empty id list said
// nothing was created, about keywords that were.
func TestCreateCampaign_PartialKeywordFailureKeepsTheCreatedIDs(t *testing.T) {
	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[701,null,703],"PartialErrors":[{"Index":1,"Code":1042,"ErrorCode":"CampaignServiceEditorialError"}]}`}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInputWithKeywords())
	if err == nil {
		t.Fatal("a PartialError must still surface as an error — some keywords were rejected")
	}
	if res == nil {
		t.Fatal("expected a partial result: the campaign, ad group and ad exist upstream")
	}
	if len(res.KeywordIDs) != 2 || res.KeywordIDs[0] != "701" || res.KeywordIDs[1] != "703" {
		t.Errorf("KeywordIDs = %v, want [701 703] — the ids that were created must be persisted", res.KeywordIDs)
	}
	// The message must say how many landed, or an operator cannot tell this from a total failure.
	if !strings.Contains(err.Error(), "2 of 3") {
		t.Errorf("error does not report the partial count: %v", err)
	}
}

// Every entry rejected IS a clean failure: there is nothing upstream to reconcile, so the id
// list stays empty and the message carries no partial count.
func TestCreateCampaign_AllKeywordsRejectedIsACleanFailure(t *testing.T) {
	api := &campaignsAPI{keywordPostBody: `{"KeywordIds":[null,null,null],"PartialErrors":[{"Index":0,"Code":1042,"ErrorCode":"CampaignServiceEditorialError"}]}`}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInputWithKeywords())
	if err == nil {
		t.Fatal("expected an error when every keyword was rejected")
	}
	if res != nil && len(res.KeywordIDs) != 0 {
		t.Errorf("KeywordIDs = %v, want empty — nothing was created", res.KeywordIDs)
	}
	if strings.Contains(err.Error(), " of ") {
		t.Errorf("a total rejection must not claim a partial count: %v", err)
	}
}

// ---- the reuse path: a found ad group is never re-keyworded -----------------

// TestCreateCampaign_ReusedAdGroupIsNotReKeyworded is the DUPLICATE-SPEND test.
//
// v13's AddKeywords has no idempotency key and v13 exposes no keyword READ, so when the ad
// group is found by name the client cannot tell which keywords already hang off it. Posting
// them anyway would attach a SECOND copy of every keyword — duplicate bids on the same terms
// and real duplicated spend. This asserts the POST is absent, and asserts it by capturing the
// request BODY: keywordSeen stays at its zero value only if nothing was ever decoded into it,
// so a client that posted an empty or partial keyword batch fails here too rather than
// sliding past a bare call-count check.
func TestCreateCampaign_ReusedAdGroupIsNotReKeyworded(t *testing.T) {
	in := validInputWithKeywords()
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)

	var keywordBody createKeywordsRequest
	api := &campaignsAPI{
		// The ad group and the ad both already exist — the full reuse path a retry takes once
		// the campaign name matches.
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
		keywordSeen:    &keywordBody,
	}
	var keywordPaths []string
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Keywords") {
			keywordPaths = append(keywordPaths, r.URL.Path)
		}
		base(w, r)
	})

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// 1. No request reached the keyword route at all.
	if len(keywordPaths) != 0 {
		t.Errorf("a reused ad group must not be re-keyworded, but %d POST(s) reached %v — every keyword would now exist twice", len(keywordPaths), keywordPaths)
	}
	// 2. The BODY assertion: nothing was ever decoded into the captured request, so no keyword
	// batch (not even an empty one) was sent.
	if keywordBody.AdGroupId != "" || len(keywordBody.Keywords) != 0 {
		t.Errorf("a keyword request body was sent on the reuse path: AdGroupId=%q Keywords=%+v", keywordBody.AdGroupId, keywordBody.Keywords)
	}
	// 3. No ids are claimed for keywords this run did not create. Reporting the ids of a prior
	// run's keywords would be a fabrication — the client never read them.
	if len(res.KeywordIDs) != 0 {
		t.Errorf("KeywordIDs = %v, want empty: this run attached no keywords", res.KeywordIDs)
	}
}

// TestCreateCampaign_ReusedAdGroupStepsExplainTheSkippedKeywords pins the OPERATOR-FACING
// half of the fix. The bug this closes was not only the duplicate POST: it was that the steps
// said the ad group was "not re-created" while its keywords silently doubled. An operator
// reading the output must learn what happened to the keywords WITHOUT opening the Bing UI —
// both that they were left alone, and that a newly-added keyword is therefore unattached.
func TestCreateCampaign_ReusedAdGroupStepsExplainTheSkippedKeywords(t *testing.T) {
	in := validInputWithKeywords()
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
	api := &campaignsAPI{
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
	}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	joined := strings.Join(res.Steps, "\n")
	// The skip is STATED, and tied to the ad group it applies to.
	if !strings.Contains(joined, "Keywords NOT re-posted") {
		t.Errorf("the steps must state that the keywords were not re-posted, got:\n%s", joined)
	}
	if !strings.Contains(joined, "111") {
		t.Errorf("the keyword-skip step must name the reused ad group id 111, got:\n%s", joined)
	}
	// The REASON is stated, so the behaviour reads as deliberate rather than as a missing step.
	if !strings.Contains(joined, "duplicate") {
		t.Errorf("the steps must say re-posting would duplicate the keywords, got:\n%s", joined)
	}
	// The RESIDUAL RISK is stated: a keyword added since the first run is not attached.
	if !strings.Contains(joined, "manually") {
		t.Errorf("the steps must tell the operator a newly-added keyword needs attaching manually, got:\n%s", joined)
	}
}

// TestCreateCampaign_CreatedAdGroupStillGetsItsKeywords is the counterpart that stops the fix
// from over-reaching. The skip is conditioned on the ad group having ALREADY EXISTED; a
// FRESHLY CREATED ad group has no keywords yet, so it must still be keyworded in full. A
// guard that skipped unconditionally would leave every new campaign inert, and would pass the
// two tests above.
func TestCreateCampaign_CreatedAdGroupStillGetsItsKeywords(t *testing.T) {
	in := validInputWithKeywords()
	var keywordBody createKeywordsRequest
	// No adGroupGetBody: the lookup returns an empty list, so the ad group is CREATED (654).
	api := &campaignsAPI{keywordSeen: &keywordBody}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// The body proves the real batch went out, against the ad group this run created.
	if got := keywordBody.AdGroupId.String(); got != "654" {
		t.Errorf("AddKeywords AdGroupId = %q, want the newly-created ad group 654", got)
	}
	if len(keywordBody.Keywords) != 3 {
		t.Fatalf("sent %d keywords, want all 3 on a freshly-created ad group: %+v", len(keywordBody.Keywords), keywordBody.Keywords)
	}
	if keywordBody.Keywords[0].Text != "kubernetes training" {
		t.Errorf("keyword[0].Text = %q, want %q", keywordBody.Keywords[0].Text, "kubernetes training")
	}
	if len(res.KeywordIDs) != 3 {
		t.Errorf("KeywordIDs = %v, want 3 ids", res.KeywordIDs)
	}
}

// TestCreateCampaign_ReusedAdGroupWithoutKeywordsStillSaysItCannotServe guards the ordering of
// the new branch against the pre-existing "no keywords supplied" message. The skip branch is
// gated on len(keywords) > 0, so a reuse with NO keyword input must fall through to the
// cannot-serve line rather than claiming keywords were preserved that were never supplied.
func TestCreateCampaign_ReusedAdGroupWithoutKeywordsStillSaysItCannotServe(t *testing.T) {
	in := validInput() // no keywords
	name := composeName(in)
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
	// EVERY level is pre-provided, including the campaign, so nothing is created this run —
	// which is what makes the AlreadyExisted assertion below meaningful.
	api := &campaignsAPI{
		getBody:        `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`,
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
	}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if strings.Contains(joined, "Keywords NOT re-posted") {
		t.Errorf("no keywords were supplied, so nothing was preserved; the skip line must not appear:\n%s", joined)
	}
	if !strings.Contains(joined, "cannot serve") {
		t.Errorf("the steps must still state the campaign cannot serve without keywords, got:\n%s", joined)
	}
	// The whole tree pre-existed and nothing was created, so this is the untouched-tree case.
	if !res.AlreadyExisted {
		t.Errorf("AlreadyExisted = false, want true: campaign, ad group and ad all pre-existed and no keyword was created")
	}
}

// TestCreateCampaign_ReusedAdGroupWithKeywordsIsNotAnUntouchedTree pins AlreadyExisted on the
// skip path. Its documented contract is "this run created NOTHING", and on this path that is
// decided by the campaign and ad levels alone — the keyword step created nothing by
// construction. A freshly-created AD under a reused group must therefore still make it false.
func TestCreateCampaign_ReusedAdGroupWithKeywordsIsNotAnUntouchedTree(t *testing.T) {
	in := validInputWithKeywords()
	name := composeName(in)
	adGroupName := composeAdGroupName(in)
	// The campaign AND the ad group are pre-provided, so the AD is the ONLY entity this run
	// creates. That isolation is the point: if the campaign were also created here, the
	// campaign level alone would force AlreadyExisted false and the assertion below would
	// hold no matter what the ad contributed — passing for the wrong reason.
	api := &campaignsAPI{
		getBody:        `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`,
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		// adGetBody is left empty: no matching ad, so the AD is CREATED this run.
	}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.AlreadyExisted {
		t.Error("AlreadyExisted = true, want false: this run created the ad, so the tree was not untouched")
	}
	if res.AdID == "" {
		t.Error("the ad should have been created on this path")
	}
}

// TestCreateCampaign_ReusedTreeWithKeywordsIsStillAnUntouchedTree pins AlreadyExisted on the
// keyword-SKIP path, where the whole tree pre-existed AND keywords were supplied.
//
// This is the case the skip branch newly reaches, and its AlreadyExisted contract is "this
// run created NOTHING". Nothing IS what this run created: the campaign, ad group and ad were
// all matched by lookup, and the keyword step deliberately posted nothing. So it must report
// true. A branch that hardcoded false here would tell the caller the run produced something
// new — inviting a reconcile for state that is already correct and consistent — and every
// other assertion about this path (no POST, the steps text, the empty KeywordIDs) would stay
// green while it did, which is exactly why this needs its own test.
func TestCreateCampaign_ReusedTreeWithKeywordsIsStillAnUntouchedTree(t *testing.T) {
	in := validInputWithKeywords()
	name := composeName(in)
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
	// Every level pre-provided by its lookup: nothing is created at any level this run.
	api := &campaignsAPI{
		getBody:        `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`,
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
	}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// No CREATE of any kind may be issued — including the keyword create.
		if r.Method == http.MethodPost && (strings.HasSuffix(p, "/Campaigns") ||
			strings.HasSuffix(p, "/AdGroups") || strings.HasSuffix(p, "/Ads") ||
			strings.HasSuffix(p, "/Keywords")) {
			t.Errorf("create POST %s issued despite every level pre-existing", p)
		}
		base(w, r)
	})

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted = false, want true: campaign, ad group and ad all pre-existed and the keyword step posted nothing, so this run created nothing")
	}
}

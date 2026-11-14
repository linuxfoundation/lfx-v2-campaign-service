// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeAudienceExpansion(t *testing.T) {
	for in, want := range map[string]string{"": "", "  ": "", "broad": "BROAD", " Defined ": "DEFINED", "EXPANDED": "EXPANDED"} {
		if got, err := NormalizeAudienceExpansion(in); err != nil || got != want {
			t.Errorf("NormalizeAudienceExpansion(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"WIDE", "BROAD,DEFINED", "LOOKALIKE"} {
		var tErr *TargetingError
		if _, err := NormalizeAudienceExpansion(in); !errors.As(err, &tErr) || tErr.Field != FieldAudienceExpansion || !strings.Contains(err.Error(), "must be BROAD, DEFINED or EXPANDED") {
			t.Errorf("NormalizeAudienceExpansion(%q) err = %v, want a refusal", in, err)
		}
	}
}

// The line item THIS call creates carries audience_expansion; without one, the parameter is absent.
func TestCreateCampaign_AudienceExpansionRidesOnTheNewLineItem(t *testing.T) {
	for in, want := range map[string]string{"broad": "BROAD", "": ""} {
		f := newTargetingFakeX()
		c := targetingClient(t, f)
		ci := targetingInput(nil, nil)
		ci.AudienceExpansion = in
		res, err := c.CreateCampaign(context.Background(), ci)
		if err != nil {
			t.Fatalf("CreateCampaign(%q): %v", in, err)
		}
		f.mu.Lock()
		raw := f.lineItemQuery
		f.mu.Unlock()
		q, _ := url.ParseQuery(raw)
		if got, has := q.Get("audience_expansion"), q.Has("audience_expansion"); got != want || has != (want != "") {
			t.Errorf("audienceExpansion %q: line-item query %q, want audience_expansion=%q", in, raw, want)
		}
		steps := strings.Join(res.Steps, "\n")
		if want != "" && !strings.Contains(steps, "Line item li1 sent with audience expansion BROAD") || want == "" && strings.Contains(steps, "audience expansion") {
			t.Errorf("audienceExpansion %q: steps = %s", in, steps)
		}
	}
}

// A reused line item keeps its own expansion: nothing is sent for it, and a warning step says so.
func TestCreateCampaign_AudienceExpansionNotAppliedToAReusedLineItem(t *testing.T) {
	f := newTargetingFakeX()
	f.reuseLineItem = "Events | KubeCon | Promoted Tweets | AUTO"
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.AudienceExpansion = "EXPANDED"
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if writes, _ := f.snapshot(); strings.Contains(strings.Join(writes, " "), "line_items") {
		t.Fatalf("writes = %v, want no line-item POST", writes)
	}
	if !res.Reused || !strings.Contains(strings.Join(res.Steps, "\n"), "audience expansion EXPANDED NOT applied to reused line item li9") {
		t.Errorf("reused=%v steps = %v", res.Reused, res.Steps)
	}
}

// A value outside X's set is refused before any request.
func TestCreateCampaign_BadAudienceExpansionSendsNothing(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.AudienceExpansion = "WIDE"
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), "invalid twitterConfig.audienceExpansion") {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}

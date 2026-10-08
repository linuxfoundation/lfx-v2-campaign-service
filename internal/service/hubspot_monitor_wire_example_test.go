// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
)

// hubspotMonitorSpecs are every generated OpenAPI document — v2 and v3, in gen/ and in the kodata
// copy the binary embeds.
var hubspotMonitorSpecs = []string{
	filepath.Join("..", "..", "gen", "http", "openapi.json"),
	filepath.Join("..", "..", "gen", "http", "openapi3.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
}

// hubspotMonitorSpecExamples returns every `example` value anywhere in the spec at rel, keyed by
// its JSON path. The whole document is walked — schemas, properties, response bodies, items —
// because Goa publishes the same example under several generated names, and property-level
// examples can describe an impossible response as easily as type-level ones.
func hubspotMonitorSpecExamples(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	out := map[string]any{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, child := range n {
				if k == "example" {
					out[path+"/example"] = child
					continue
				}
				walk(path+"/"+k, child)
			}
		case []any:
			for i, child := range n {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("", doc)
	return out
}

// hubspotMonitorVisit calls fn on v and every object and array nested in it.
func hubspotMonitorVisit(v any, fn func(any)) {
	fn(v)
	switch n := v.(type) {
	case map[string]any:
		for _, child := range n {
			hubspotMonitorVisit(child, fn)
		}
	case []any:
		for _, child := range n {
			hubspotMonitorVisit(child, fn)
		}
	}
}

var hubspotMonitorCounters = []string{"sent", "delivered", "opens", "clicks", "bounces", "unsubscribes", "spam_reports"}

// hubspotMonitorRates maps each published rate to its numerator and denominator counters.
var hubspotMonitorRates = map[string][2]string{
	"open_rate":        {"opens", "delivered"},
	"click_rate":       {"clicks", "delivered"},
	"bounce_rate":      {"bounces", "sent"},
	"unsubscribe_rate": {"unsubscribes", "delivered"},
	"spam_rate":        {"spam_reports", "delivered"},
}

// checkHubSpotCounterObject pins one email row or totals object: every counter present, every rate
// present exactly when its denominator is non-zero and equal to the quotient, and no cost field —
// HubSpot bills nothing per send.
func checkHubSpotCounterObject(t *testing.T, path string, n map[string]any) {
	t.Helper()
	for _, c := range hubspotMonitorCounters {
		if _, ok := n[c].(float64); !ok {
			t.Errorf("%s: counter %s missing or not a number", path, c)
		}
	}
	for r, nd := range hubspotMonitorRates {
		num, _ := n[nd[0]].(float64)
		den, _ := n[nd[1]].(float64)
		got, present := n[r].(float64)
		if den == 0 {
			if _, has := n[r]; has {
				t.Errorf("%s: %s present over a zero %s", path, r, nd[1])
			}
			continue
		}
		if !present || math.Abs(got-num/den) > 1e-12 {
			t.Errorf("%s: %s = %v, want %s/%s = %v", path, r, n[r], nd[0], nd[1], num/den)
		}
	}
	for _, f := range []string{"spend", "cost", "cost_micros", "cpc", "cpm"} {
		if _, has := n[f]; has {
			t.Errorf("%s: carries a cost field %q", path, f)
		}
	}
}

func checkHubSpotEnvelope(t *testing.T, path string, n map[string]any) {
	t.Helper()
	emails, _ := n["emails"].([]any)
	checked, _ := n["emails_checked"].(float64)
	notSent, _ := n["emails_not_sent_in_window"].(float64)
	if int(checked) != len(emails)+int(notSent) {
		t.Errorf("%s: emails_checked %v != %d emails + %v not sent", path, checked, len(emails), notSent)
	}
	totals, ok := n["totals"].(map[string]any)
	if !ok {
		t.Fatalf("%s: envelope without totals", path)
	}
	if c, _ := totals["email_count"].(float64); int(c) != len(emails) {
		t.Errorf("%s: totals.email_count %v beside %d emails", path, totals["email_count"], len(emails))
	}
	ids := map[string]bool{}
	for _, c := range hubspotMonitorCounters {
		var sum float64
		for _, e := range emails {
			sum += e.(map[string]any)[c].(float64)
		}
		if got, _ := totals[c].(float64); got != sum {
			t.Errorf("%s: totals.%s = %v, want the emails' sum %v", path, c, totals[c], sum)
		}
	}
	for _, e := range emails {
		m := e.(map[string]any)
		ids[fmt.Sprint(m["email_id"])+"|"+fmt.Sprint(m["campaign_id"])] = true
	}
	items, _ := n["action_items"].([]any)
	for i, it := range items {
		m := it.(map[string]any)
		if key := fmt.Sprint(m["email_id"]) + "|" + fmt.Sprint(m["campaign_id"]); !ids[key] {
			t.Errorf("%s: action_items[%d] (email_id|campaign_id %s) joins no row in emails", path, i, key)
		}
	}
	// The published findings must be EXACTLY what the rule engine produces for the published
	// emails — an example finding the rules would never raise is an impossible response.
	var modelEmails []model.HubSpotMonitorEmail
	for _, e := range emails {
		m := e.(map[string]any)
		num := func(k string) int64 { f, _ := m[k].(float64); return int64(f) }
		modelEmails = append(modelEmails, model.HubSpotMonitorEmail{
			CampaignID: fmt.Sprint(m["campaign_id"]), EmailID: fmt.Sprint(m["email_id"]), Name: fmt.Sprint(m["name"]),
			Counters: model.HubSpotEmailCounters{
				Sent: num("sent"), Delivered: num("delivered"), Opens: num("opens"), Clicks: num("clicks"),
				Bounces: num("bounces"), Unsubscribes: num("unsubscribes"), SpamReports: num("spam_reports"),
			},
		})
	}
	want := rules.EvaluateHubSpotMonitor(modelEmails)
	if len(want) != len(items) {
		t.Errorf("%s: %d published action items, the rules produce %d: %+v", path, len(items), len(want), want)
	} else {
		for i, w := range want {
			got := items[i].(map[string]any)
			if got["priority"] != string(w.Priority) || got["issue"] != w.Issue || got["action"] != w.Action ||
				got["campaign_id"] != w.CampaignID || got["email_id"] != w.EmailID || got["campaign_name"] != w.CampaignName {
				t.Errorf("%s: action_items[%d] = %v, the rules produce %+v", path, i, got, w)
			}
		}
	}
	_, asOf := n["metrics_as_of"]
	ws, hasStart := n["metrics_window_start"].(string)
	we, hasEnd := n["metrics_window_end"].(string)
	if asOf != hasStart || hasStart != hasEnd {
		t.Errorf("%s: metrics_as_of/window_start/window_end must be all present or all absent", path)
	}
	if hasStart && hasEnd {
		s, err1 := time.Parse(time.DateOnly, ws)
		e, err2 := time.Parse(time.DateOnly, we)
		days, _ := n["days"].(float64)
		if err1 != nil || err2 != nil || int(e.Sub(s).Hours()/24)+1 != int(days) {
			t.Errorf("%s: window %s..%s does not cover days=%v", path, ws, we, n["days"])
		}
	}
}

// TestPublishedHubSpotMonitorExamplesArePossible pins that no example Goa publishes for the
// HubSpot email monitor describes a response the endpoint cannot return: rates that do not follow
// from the counters beside them, totals that are not the sum of the emails, a completeness count
// that does not add up, a finding about an email not in the list, a window that does not cover
// `days`, or any cost field.
func TestPublishedHubSpotMonitorExamplesArePossible(t *testing.T) {
	for _, rel := range hubspotMonitorSpecs {
		t.Run(rel, func(t *testing.T) {
			sawRow, sawTotals, sawEnvelope := false, false, false
			for path, ex := range hubspotMonitorSpecExamples(t, rel) {
				hubspotMonitorVisit(ex, func(v any) {
					n, ok := v.(map[string]any)
					if !ok {
						return
					}
					switch {
					case n["emails_checked"] != nil:
						sawEnvelope = true
						checkHubSpotEnvelope(t, path, n)
					case n["email_id"] != nil && n["spam_reports"] != nil:
						sawRow = true
						checkHubSpotCounterObject(t, path, n)
					case n["email_count"] != nil:
						sawTotals = true
						checkHubSpotCounterObject(t, path, n)
					}
				})
			}
			// Guard against the walk silently finding nothing (a renamed field, a moved spec).
			if !sawRow || !sawTotals || !sawEnvelope {
				t.Fatalf("found row=%v totals=%v envelope=%v HubSpot monitor examples; want all three", sawRow, sawTotals, sawEnvelope)
			}
		})
	}
}

// TestPublishedAdMonitorActionItemExamplesCarryNoEmailID pins that no published example of an
// AD-monitor finding carries email_id, which only monitor-hubspot-account sets: a Google-style
// campaign id and underspend issue beside an email id is a finding no monitor can produce. Goa
// clones attribute examples into the shared type's example, so this lived in the generated spec.
func TestPublishedAdMonitorActionItemExamplesCarryNoEmailID(t *testing.T) {
	for _, rel := range hubspotMonitorSpecs {
		t.Run(rel, func(t *testing.T) {
			sawAd, sawHubSpot := false, false
			for path, ex := range hubspotMonitorSpecExamples(t, rel) {
				hubspotMonitorVisit(ex, func(v any) {
					n, ok := v.(map[string]any)
					if !ok || n["priority"] == nil || n["issue"] == nil {
						return
					}
					_, hasEmail := n["email_id"]
					hubspotFinding := strings.Contains(fmt.Sprint(n["issue"]), "Bounce rate") ||
						strings.Contains(fmt.Sprint(n["issue"]), "delivered")
					if hubspotFinding {
						sawHubSpot = true
						return
					}
					sawAd = true
					if hasEmail {
						t.Errorf("%s: ad-monitor finding %v carries email_id", path, n)
					}
				})
			}
			if !sawAd || !sawHubSpot {
				t.Fatalf("found ad=%v hubspot=%v action-item examples; want both", sawAd, sawHubSpot)
			}
		})
	}
}

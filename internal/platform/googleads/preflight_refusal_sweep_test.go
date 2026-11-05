// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// sweepSentinel marks caller text in the sweep below. It carries a space and a query string,
// so domain.CallerValue never shows it, and its shapes reach past each validator's first
// check: plain free text, a resource name on the test account, and a URL.
const sweepSentinel = "SENTINEL494"

// sweepOwnerDigits is a caller-supplied customer id with no letters at all: an opaque 40-digit
// run that CallerValue withholds once quoted, but that a free-text sentinel can't stand in for,
// since the resource-name validators only reach their owner check with digits there (#494
// review: the owner was echoed with %s).
const sweepOwnerDigits = "4949494949494949494949494949494949494949"

var sweepValues = []string{
	sweepSentinel + " x?token=y",
	"customers/1234567890/" + sweepSentinel + " x?token=y/1",
	"customers/1234567890/userLists/" + sweepSentinel + "?token=y",
	"https://example.com/" + sweepSentinel + "?token=y",
	// A malformed URL long enough that a capped error loses its closing quote.
	"https://example.com:" + sweepSentinel + "-" + strings.Repeat("x", 180) + "/image.png",
	// Another account's resource, owner id digits only, in each collection a validator owns.
	"customers/" + sweepOwnerDigits + "/conversionActions/1",
	"customers/" + sweepOwnerDigits + "/audiences/1",
	"customers/" + sweepOwnerDigits + "/userLists/1",
	"customers/" + sweepOwnerDigits + "/customAudiences/1",
	"customers/" + sweepOwnerDigits + "/sharedSets/1",
}

// sweepSetNth walks v and sets its n-th caller string slot to value, counting every slot it
// passes in *seen. A string field is one slot; each element of a []string is one, and an
// empty []string is one (filled with a single entry when chosen). An empty slice of structs
// or a nil struct pointer is walked as one would-be element, kept only when the chosen slot
// fell inside it, so the slot numbering is the same on every run. Maps, byte slices and
// unexported fields are skipped.
func sweepSetNth(v reflect.Value, n int, seen *int, value string, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if *seen == n {
			v.SetString(value)
		}
		*seen++
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				sweepSetNth(f, n, seen, value, depth+1)
			}
		}
	case reflect.Pointer:
		if v.Type().Elem().Kind() != reflect.Struct {
			return
		}
		if !v.IsNil() {
			sweepSetNth(v.Elem(), n, seen, value, depth+1)
			return
		}
		before := *seen
		elem := reflect.New(v.Type().Elem())
		sweepSetNth(elem.Elem(), n, seen, value, depth+1)
		if n >= before && n < *seen {
			v.Set(elem)
		}
	case reflect.Slice:
		et := v.Type().Elem()
		switch {
		case et.Kind() == reflect.String:
			if v.Len() == 0 {
				if *seen == n {
					v.Set(reflect.ValueOf([]string{value}).Convert(v.Type()))
				}
				*seen++
				return
			}
			for i := 0; i < v.Len(); i++ {
				sweepSetNth(v.Index(i), n, seen, value, depth+1)
			}
		case et.Kind() == reflect.Struct:
			if v.Len() > 0 {
				for i := 0; i < v.Len(); i++ {
					sweepSetNth(v.Index(i), n, seen, value, depth+1)
				}
				return
			}
			before := *seen
			elem := reflect.New(et).Elem()
			sweepSetNth(elem, n, seen, value, depth+1)
			if n >= before && n < *seen {
				v.Set(reflect.Append(v, elem))
			}
		}
	}
}

// TestPreflightCampaignKind_NoCallerTextReachesTheOperatorMessage is the class guard for
// WrapQuotedPreflightRefusal's premise (#494): a pre-flight refusal formats caller text only
// quoted, or after a check has narrowed it to a safe alphabet. It puts the sentinel, in each of
// its shapes, into every caller string slot of each channel's input, one slot at a time, and
// fails if any refusal's operator message shows it. A validator that formats a raw caller
// segment with %s is caught here even when no hand-written case names it.
func TestPreflightCampaignKind_NoCallerTextReachesTheOperatorMessage(t *testing.T) {
	c := &Client{account: testAccount()}
	bases := []struct {
		kind string
		in   func() CampaignInput
	}{
		{campaignKindSearch, sampleInput},
		{campaignKindDemandGen, demandGenInput},
		{campaignKindPerformanceMax, func() CampaignInput { return pmaxInput(fullPMaxCreative()) }},
		{campaignKindDisplay, func() CampaignInput { return displayCreativeInput(fullDisplayCreative()) }},
		{campaignKindVideo, sampleInput},
	}
	refusals := 0
	for _, b := range bases {
		total := 0
		probe := b.in()
		sweepSetNth(reflect.ValueOf(&probe).Elem(), -1, &total, "", 0)
		if total == 0 {
			t.Fatalf("%s: the sweep found no caller string slots", b.kind)
		}
		for n := 0; n < total; n++ {
			for _, value := range sweepValues {
				in := b.in()
				seen := 0
				sweepSetNth(reflect.ValueOf(&in).Elem(), n, &seen, value, 0)
				_, err := c.preflightCampaignKind(b.kind, in)
				if err == nil {
					continue
				}
				msg, ok := domain.AsPreflightRefusal(err)
				if !ok {
					continue
				}
				refusals++
				if strings.Contains(msg, sweepSentinel) || strings.Contains(msg, "token=") || strings.Contains(msg, sweepOwnerDigits) {
					t.Errorf("%s slot %d with %q: the operator message shows caller text: %s", b.kind, n, value, msg)
				}
			}
		}
	}
	// The sweep proves nothing if no slot ever reaches a refusal.
	if refusals < 50 {
		t.Fatalf("only %d sentinel inputs were refused; the sweep is not reaching the validators", refusals)
	}
}

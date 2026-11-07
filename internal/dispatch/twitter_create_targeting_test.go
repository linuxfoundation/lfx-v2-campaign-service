// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

func twitterTargetingCfg(extra string) json.RawMessage {
	return json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"` + extra + `}}`)
}

// twitterTargetingServer answers a full create plus the targeting lookups, recording each
// criterion POST as "<type>=<value>" and counting every request.
func twitterTargetingServer(t *testing.T) (*TwitterDispatcher, func() []string, *atomic.Int64) {
	t.Helper()
	var mu sync.Mutex
	var criteria []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/12/targeting_criteria/locations":
			code := q.Get("country_code")
			_, _ = w.Write([]byte(`{"data":[{"country_code":"` + code + `","location_type":"COUNTRIES","targeting_type":"LOCATION","targeting_value":"loc-` + strings.ToLower(code) + `"}],"next_cursor":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/12/targeting_criteria/languages":
			_, _ = w.Write([]byte(`{"data":[{"targeting_type":"LANGUAGE","targeting_value":"en"}],"next_cursor":null}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
			mu.Lock()
			criteria = append(criteria, q.Get("targeting_type")+"="+q.Get("targeting_value"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"id":"tc1","line_item_id":"` + q.Get("line_item_id") + `","targeting_type":"` + q.Get("targeting_type") + `","targeting_value":"` + q.Get("targeting_value") + `"}}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return d, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), criteria...) }, &calls
}

// Off (the default): either list is a synchronous 400 naming the flag, and Dispatch refuses it
// again, pre-create, before any X call.
func TestTwitterCreateTargeting_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "")
	d, _, calls := twitterTargetingServer(t)
	for _, extra := range []string{`,"geoTargets":["US"]`, `,"languages":["en"]`, `,"geoTargets":["US"],"languages":["en"]`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra))
		requireConfigInvalid(t, err, constants.EnvTwitterCreateTargetingEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"geoTargets":["US"]`))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvTwitterCreateTargetingEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d X calls before the gate refused; want 0", n)
	}
	// Empty lists and null are absent: nothing to gate.
	for _, extra := range []string{``, `,"geoTargets":[]`, `,"languages":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a malformed list or a wrong JSON type is the synchronous 400 naming the field, even beside
// an unrelated mistyped field.
func TestTwitterCreateTargeting_PreflightRefusesBadLists(t *testing.T) {
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	d, _, _ := twitterTargetingServer(t)
	for extra, want := range map[string]string{
		`,"geoTargets":["USA"]`:                       "twitterConfig.geoTargets",
		`,"geoTargets":"US"`:                          "twitterConfig.geoTargets: must be an array of strings",
		`,"languages":[1]`:                            "twitterConfig.languages: must be an array of strings",
		`,"languages":[" "]`:                          "twitterConfig.languages",
		`,"budgetAmount":"bad","languages":["e n"]`:   "twitterConfig.languages",
		`,"budgetAmount":"bad","GEOTARGETS":["USA1"]`: "twitterConfig.geoTargets",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra))
		requireConfigInvalid(t, err, want)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"geoTargets":["us"],"languages":["EN"]`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid lists: PreflightCreate = %v, want not applicable", err)
	}
}

// On: the normalized lists reach the client, which targets the new line item.
func TestTwitterCreateTargeting_DispatchSendsTheCriteria(t *testing.T) {
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	d, criteria, _ := twitterTargetingServer(t)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"geoTargets":["us","CA","US"],"languages":["EN"]`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	if got := strings.Join(criteria(), ","); got != "LOCATION=loc-us,LOCATION=loc-ca,LANGUAGE=en" {
		t.Fatalf("criteria = %q", got)
	}
}

// Dispatch names the field for a mistyped list, before the generic decode error can.
func TestTwitterCreateTargeting_DispatchNamesAMistypedList(t *testing.T) {
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	d, _, calls := twitterTargetingServer(t)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"geoTargets":"US"`))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), "twitterConfig.geoTargets: must be an array of strings") {
		t.Fatalf("err = %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d X calls; want 0", n)
	}
}

// A criterion that fails part-way (#567 review) reaches the job as a classified, upstream-text-free
// message, and the partial Result carries the targeting specifics (applied count, failed criterion,
// do-not-enable warning) as a step, since DispatchFailureMessage never echoes the error's text.
func TestTwitterCreateTargeting_CriterionFailureReachesTheJobClassified(t *testing.T) {
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	const upstream = "UPSTREAM-BODY-MARKER"
	var promoted atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/12/targeting_criteria/locations":
			_, _ = w.Write([]byte(`{"data":[{"country_code":"US","location_type":"COUNTRIES","targeting_type":"LOCATION","targeting_value":"loc-us"}],"next_cursor":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/12/targeting_criteria/languages":
			_, _ = w.Write([]byte(`{"data":[{"targeting_type":"LANGUAGE","targeting_value":"en"}],"next_cursor":null}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
			if q.Get("targeting_type") == "LANGUAGE" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":[{"message":"` + upstream + `"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"tc1","line_item_id":"` + q.Get("line_item_id") + `","targeting_type":"` + q.Get("targeting_type") + `","targeting_value":"` + q.Get("targeting_value") + `"}}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			promoted.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"geoTargets":["US"],"languages":["en"]`))
	if err == nil || camp == nil {
		t.Fatalf("Dispatch = (%v, %v), want a partial and an error", camp, err)
	}
	if !errors.Is(err, twitter.ErrCreateTargetingIncomplete) {
		t.Errorf("err does not carry ErrCreateTargetingIncomplete: %v", err)
	}
	if n := promoted.Load(); n != 0 {
		t.Errorf("%d promoted_tweets calls after a targeting failure; want 0", n)
	}

	msg := service.DispatchFailureMessage(model.ProviderTwitterAds, err, false)
	if !strings.Contains(msg, "created the campaign but could not confirm its setup finished") {
		t.Errorf("job message = %q, want the setup-incomplete wording", msg)
	}
	if strings.Contains(msg, upstream) {
		t.Errorf("job message carries upstream text: %q", msg)
	}

	var blob struct {
		LineItemID string   `json:"line_item_id"`
		Steps      []string `json:"steps"`
	}
	if jerr := json.Unmarshal(camp.Result, &blob); jerr != nil {
		t.Fatalf("Result: %v", jerr)
	}
	last := ""
	if len(blob.Steps) > 0 {
		last = blob.Steps[len(blob.Steps)-1]
	}
	for _, want := range []string{"Targeting INCOMPLETE on line item li1", "1 of 2 criteria applied", "language en failed", "Do not enable the line item"} {
		if !strings.Contains(last, want) {
			t.Errorf("last step = %q, want it to contain %q", last, want)
		}
	}
	if strings.Contains(strings.Join(blob.Steps, "\n"), upstream) {
		t.Errorf("steps carry upstream text: %v", blob.Steps)
	}
}

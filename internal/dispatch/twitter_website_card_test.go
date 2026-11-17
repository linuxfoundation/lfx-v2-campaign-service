// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Gap 6 (#564): card sits behind its own default-off flag (a synchronous 400 naming it, refused
// again at dispatch before any X call), and a wrong shape, a bad headline, a missing image or a
// create that authors no tweet is a synchronous 400.
func TestTwitterWebsiteCard_GateShapeAndRules(t *testing.T) {
	t.Setenv(constants.EnvTwitterWebsiteCardEnabled, "")
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefAsset(xTestPNG(900)), nil)
	cfg := xImageConfig(`,"card":{"headline":"Register"}`)
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, cfg), constants.EnvTwitterWebsiteCardEnabled)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	requirePreCreate(t, err)
	x.mu.Lock()
	total := x.total
	x.mu.Unlock()
	if total != 0 {
		t.Fatalf("%d X calls with the flag off; want 0", total)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, xImageConfig(`,"card":null`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("null card with the flag off: %v, want not applicable", err)
	}

	t.Setenv(constants.EnvTwitterWebsiteCardEnabled, "true")
	noImage := `{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetText":"Join","card":{"headline":"h"}}}`
	withTweet := `{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"123","card":{"headline":"h"}}}`
	for raw, want := range map[string]string{
		string(xImageConfig(`,"card":"Register"`)):                                     "twitterConfig.card: must be a",
		string(xImageConfig(`,"card":{"headline":1}`)):                                 "twitterConfig.card: headline must be a string",
		string(xImageConfig(`,"card":{"headline":"h","url":"x"}`)):                     "twitterConfig.card: has an unknown key",
		string(xImageConfig(`,"card":{"headline":" "}`)):                               "twitterConfig.card: headline is blank",
		string(xImageConfig(`,"card":{"headline":"` + strings.Repeat("h", 71) + `"}`)): "at most 70 characters",
		noImage:   "twitterConfig.card: a website card needs media",
		withTweet: "twitterConfig.card: a website card rides on an authored tweet",
	} {
		requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, json.RawMessage(raw)), want)
	}
}

// On: the card is created and attached to the authored tweet, and recorded on the row.
func TestTwitterWebsiteCard_DispatchAttachesTheCard(t *testing.T) {
	t.Setenv(constants.EnvTwitterWebsiteCardEnabled, "true")
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefAsset(xTestPNG(900)), nil)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig(`,"card":{"headline":"Register for KubeCon"}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	x.mu.Lock()
	q := x.tweetQuery
	x.mu.Unlock()
	if x.count("card") != 1 || !strings.Contains(q, "card_uri=card%3A%2F%2F987") {
		t.Fatalf("cards=%d tweet query=%q, want one card attached by card_uri", x.count("card"), q)
	}
	if camp.Status != campaignStatusCreated || !strings.Contains(string(camp.Result), `"CardURI":"card://987"`) {
		t.Errorf("status=%q result=%s", camp.Status, camp.Result)
	}
}

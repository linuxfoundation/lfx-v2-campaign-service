// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// #494: every refusal of the caller's input that preflightCampaignKind raises is marked
// operator-safe, so the job result names the rule broken instead of "platform campaign creation
// failed". One case per kind of check, across the channels, including the one that failed in
// prod (a Performance Max create whose descriptions are all wider than 60).
func TestPreflightCampaignKind_InputRefusalsAreOperatorSafe(t *testing.T) {
	c := &Client{account: testAccount()}
	longDescriptions := []string{strings.Repeat("d", 61), strings.Repeat("e", 70)}

	cases := []struct {
		name string
		kind string
		in   func() CampaignInput
		want string
	}{
		{
			name: "performance max with no description of 60 or less",
			kind: campaignKindPerformanceMax,
			in: func() CampaignInput {
				p := fullPMaxCreative()
				p.Descriptions = longDescriptions
				return pmaxInput(p)
			},
			want: "needs at least one description with a display width of 60 or less",
		},
		{
			name: "end date before start date",
			kind: campaignKindSearch,
			in: func() CampaignInput {
				in := sampleInput()
				in.StartDate, in.EndDate = "2099-03-10", "2099-03-01"
				return in
			},
			want: "google-ads campaign end date 2099-03-01 must not be before start date 2099-03-10",
		},
		{
			name: "unknown bidding strategy",
			kind: campaignKindSearch,
			in: func() CampaignInput {
				in := sampleInput()
				in.BiddingStrategy = "bogus"
				return in
			},
			want: `google-ads: unknown bidding strategy "bogus"`,
		},
		{
			name: "display creative with no long headline",
			kind: campaignKindDisplay,
			in: func() CampaignInput {
				d := fullDisplayCreative()
				d.LongHeadline = ""
				return displayCreativeInput(d)
			},
			want: "long headline",
		},
		{
			name: "geo target that is not a country",
			kind: campaignKindSearch,
			in: func() CampaignInput {
				in := sampleInput()
				in.GeoTargets = []string{"ZZ"}
				return in
			},
			want: `google-ads: geo target "ZZ" is neither a supported country code`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.preflightCampaignKind(tc.kind, tc.in())
			if err == nil {
				t.Fatal("expected a pre-flight refusal")
			}
			var r *domain.PreflightRefusal
			if !errors.As(err, &r) {
				t.Fatalf("the refusal is not a domain.PreflightRefusal, so the job reports the generic string; err = %v", err)
			}
			if got := r.OperatorMessage(); !strings.Contains(got, tc.want) {
				t.Errorf("operator message = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// A caller value the validators quote can carry a secret: a headline is free text, and an
// image URL's path survives redactURLForError. Error() keeps it for the logs, as before #494;
// the operator message must not. The Error() assertions are what keep the leak checks from
// passing vacuously.
func TestPreflightCampaignKind_OperatorMessageWithholdsQuotedCallerValues(t *testing.T) {
	c := &Client{account: testAccount()}
	const secret = "SECRET-494"
	secretURL := "https://cdn.example.org/reset/" + secret + "/m.png"

	cases := map[string]CampaignInput{
		"a headline holding a URL": func() CampaignInput {
			p := fullPMaxCreative()
			p.Headlines[0] = "https://x.example/reset/" + secret + "/now"
			return pmaxInput(p)
		}(),
		"an image URL listed twice": func() CampaignInput {
			p := fullPMaxCreative()
			p.MarketingImages = []string{secretURL, secretURL}
			return pmaxInput(p)
		}(),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.preflightCampaignKind(campaignKindPerformanceMax, in)
			if err == nil {
				t.Fatal("expected a pre-flight refusal")
			}
			if !strings.Contains(err.Error(), secret) {
				t.Fatalf("the refusal no longer quotes the value at all, so this test proves nothing: %v", err)
			}
			msg, ok := domain.AsPreflightRefusal(err)
			if !ok {
				t.Fatalf("not a marked refusal: %v", err)
			}
			for _, leak := range []string{secret, "://", "/reset/"} {
				if strings.Contains(msg, leak) {
					t.Errorf("operator message %q contains %q from the caller's input", msg, leak)
				}
			}
			if !strings.Contains(msg, "(value withheld)") {
				t.Errorf("operator message %q should say the value was withheld", msg)
			}
		})
	}
}

// The connection's own account ids are the connection's fault, not the operator's input, so a
// bad stored customer id stays unmarked: the job reports it by classification, not verbatim.
func TestPreflightCampaignKind_AccountIDFailureIsNotARefusal(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "not-a-customer-id"}}
	_, err := c.preflightCampaignKind(campaignKindSearch, sampleInput())
	if err == nil {
		t.Fatal("expected the account id check to fail")
	}
	if _, ok := domain.AsPreflightRefusal(err); ok {
		t.Errorf("an account id failure was marked operator-safe: %v", err)
	}
}

// imageFetchRefusal marks what the slot refused and nothing else.
func TestImageFetchRefusal(t *testing.T) {
	slot := performanceMaxImageSlots[pmaxSlotMarketing]
	geometry := checkImageGeometry(slot, ResolvedImageRefPrefix+"11111111-1111-4111-8111-111111111111", "png", 100, 100)
	if geometry == nil {
		t.Fatal("a 100x100 marketing image must fail its slot's minimum")
	}
	transport := errors.New("google-ads marketing image could not be downloaded: " + redactedCause{errors.New("dial tcp: lookup host.example: no such host")}.Error())
	transport = wrapTransport(transport)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if got := imageFetchRefusal(context.Background(), nil); got != nil {
		t.Errorf("nil = %v, want nil", got)
	}
	if msg, ok := domain.AsPreflightRefusal(imageFetchRefusal(context.Background(), geometry)); !ok {
		t.Error("a geometry refusal must be marked operator-safe")
	} else if !strings.Contains(msg, "below the 600x314 minimum") {
		t.Errorf("geometry operator message = %q", msg)
	}
	if _, ok := domain.AsPreflightRefusal(imageFetchRefusal(context.Background(), transport)); ok {
		t.Error("a transport failure carries net/http text and must not be marked operator-safe")
	}
	if _, ok := domain.AsPreflightRefusal(imageFetchRefusal(cancelled, geometry)); ok {
		t.Error("a fetch the job's own context ended was not refused and must not be marked")
	}
}

// wrapTransport chains a redactedCause the way fetchOneImage's %w does.
func wrapTransport(err error) error {
	return &chained{msg: err.Error(), cause: redactedCause{err}}
}

type chained struct {
	msg   string
	cause error
}

func (c *chained) Error() string { return c.msg }
func (c *chained) Unwrap() error { return c.cause }

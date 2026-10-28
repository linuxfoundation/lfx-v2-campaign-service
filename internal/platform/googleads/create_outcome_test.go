// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestCreateOutcome_ClassifiesEveryPartialShape pins one case per branch of CreateOutcome.
//
// The partials are built the way the create paths build them — pre-budget carries names only,
// post-budget adds CampaignBudgetID, post-campaign adds CampaignID — because that escalating
// contract is the whole basis for classifying centrally instead of per arm.
func TestCreateOutcome_ClassifiesEveryPartialShape(t *testing.T) {
	namePartial := &CampaignResult{Platform: "google-ads", CampaignName: "c", CampaignBudgetName: "b"}
	budgetPartial := &CampaignResult{Platform: "google-ads", CampaignName: "c", CampaignBudgetName: "b", CampaignBudgetID: "111"}
	campaignPartial := &CampaignResult{Platform: "google-ads", CampaignName: "c", CampaignBudgetName: "b", CampaignBudgetID: "111", CampaignID: "222"}

	budgetDup := &apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"DUPLICATE_NAME"}}
	campaignDup := &apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"DUPLICATE_CAMPAIGN_NAME"}}
	definite4xx := &apiError{StatusCode: 400, Method: http.MethodPost}
	ambiguous5xx := &apiError{StatusCode: 503, Method: http.MethodPost}
	// A refusal whose body PARSED: the codes are what rules out a duplicate-name refusal, and
	// so what lets the budget arm claim no campaign of the composed name exists.
	refused4xx := &apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"REQUIRED_FIELD_MISSING"}}

	cases := []struct {
		name   string
		result *CampaignResult
		err    error
		want   string
		wantOK bool
	}{
		{"success is not an outcome", campaignPartial, nil, "", false},
		{"pre-create failure is not an outcome", nil, definite4xx, "", false},

		{"budget name taken", namePartial, budgetDup, domain.CreateOutcomeBudgetNameTaken, true},
		{"campaign name taken", budgetPartial, campaignDup, domain.CreateOutcomeCampaignNameTaken, true},

		{"budget created, campaign refused", budgetPartial, refused4xx, domain.CreateOutcomeBudgetCreatedCampaignRefused, true},
		{"budget created, campaign unconfirmed", budgetPartial, ambiguous5xx, domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true},
		{"budget created, transport failure is unconfirmed", budgetPartial, &transportError{Method: http.MethodPost, Err: io.ErrUnexpectedEOF}, domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true},

		{"campaign created, cascade incomplete", campaignPartial, definite4xx, domain.CreateOutcomeCampaignCreatedSetupIncomplete, true},
		{"no ids at all is unconfirmed", namePartial, ambiguous5xx, domain.CreateOutcomeUnconfirmed, true},

		// The ten arms across the five channels that hand back a budget partial with a plain
		// fmt.Errorf — no apiError and no transportError anywhere in the chain. Absence of
		// ambiguity is NOT evidence of a refusal, and in the first case below the campaign
		// provably DOES exist: the create paths only reach it after campaigns:mutate answered
		// 2xx. Reporting "refused" here would tell the operator to delete the budget of a live
		// billable campaign and retry into a second one.
		{"2xx whose resource name would not parse is unconfirmed, not refused", budgetPartial,
			fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget 111 created): %w", errors.New("no resource name in mutate response")),
			domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true},
		{"context cancelled before the campaign mutate is unconfirmed, not refused", budgetPartial,
			fmt.Errorf("google-ads creation aborted after budget 111 created: %w", context.Canceled),
			domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true},

		// DUPLICATE_NAME is not exclusive to CampaignBudgetError: parseErrorCodes keeps the
		// bare enum value and drops the error family, and PMax's assetGroups:mutate runs after
		// the campaign exists. The ids outrank the code everywhere past the budget step.
		{"duplicate name past the budget step does not claim nothing was created", campaignPartial, budgetDup,
			domain.CreateOutcomeCampaignCreatedSetupIncomplete, true},
		{"duplicate campaign name with no budget id claims no budget", namePartial, campaignDup,
			domain.CreateOutcomeUnconfirmed, true},

		// A definite 4xx whose codes did not parse. doRequest builds exactly this apiError —
		// status known, ErrorCodes nil — whenever parseErrorCodes finds no GoogleAdsFailure
		// detail: an unreadable or over-maxResponseBytes body (client.go), and equally a
		// readable envelope that carries no such detail, which is what a 401, a 403 or a
		// frontend INVALID_ARGUMENT looks like. The duplicate-campaign-name arm above cannot
		// fire on any of them, so an earlier attempt's campaign could be standing while this
		// one reports "no campaign exists" and sends the operator to delete the budget. The
		// claim needs codes it could read, not merely a status it could see.
		{"definite 4xx whose codes did not parse is unconfirmed, not refused", budgetPartial, definite4xx,
			domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true},

		// The cascade's LAST mutate answering 2xx with a response this client cannot read:
		// pmax_creative.go's assetGroupAssets:mutate and demandgen_creative.go's
		// adGroupAds:mutate both return a campaign partial with a plain fmt.Errorf whose own
		// text says the resources MAY exist. The token is right — the campaign exists — but
		// it is the reason DispatchFailureMessage says the setup could not be CONFIRMED
		// rather than that it is unfinished: past that line the step DID commit.
		{"malformed 2xx on the final mutate still classifies by the campaign id", campaignPartial,
			errors.New("google-ads Performance Max asset group link creation UNCONFIRMED (asset group 333 created; 2xx with a malformed/short mutate response for 4 link(s) — some may exist — verify in Google Ads before retrying)"),
			domain.CreateOutcomeCampaignCreatedSetupIncomplete, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CreateOutcome(tc.result, tc.err)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("CreateOutcome = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestCreateOutcome_ReadsThroughTheWrappedChain proves the classifier survives the %w wrapping
// every partial-returning arm applies — which is why no arm needs to be touched.
//
// The duplicate-name cases are here and not only in the table above because a bare apiError is
// NOT the shape that reaches this function in production: the create paths wrap before
// returning, and every predicate involved reaches its target with errors.As. A direct type
// assertion anywhere in that chain would pass the table and still never fire against the live
// platform — which is the exact case the operator hits on a retry after an orphaned budget.
func TestCreateOutcome_ReadsThroughTheWrappedChain(t *testing.T) {
	cases := []struct {
		name   string
		result *CampaignResult
		err    error
		want   string
	}{
		{
			name:   "budget created, campaign refused",
			result: &CampaignResult{CampaignBudgetID: "111"},
			err: fmt.Errorf("google-ads performance max campaign creation failed (budget 111 created): %w",
				fmt.Errorf("campaigns:mutate: %w",
					&apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"REQUIRED_FIELD_MISSING"}})),
			want: domain.CreateOutcomeBudgetCreatedCampaignRefused,
		},
		{
			// The retry-after-an-orphan case: the budget step is refused for reusing the
			// name an earlier attempt left behind, so no id comes back at all and the
			// error code is the only thing that distinguishes "nothing was created" from
			// "the budget create was sent and its outcome is unknown".
			name:   "budget name taken",
			result: &CampaignResult{CampaignName: "c", CampaignBudgetName: "b"},
			err: fmt.Errorf("google-ads performance max campaign creation failed: %w",
				fmt.Errorf("campaignBudgets:mutate: %w",
					&apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"DUPLICATE_NAME"}})),
			want: domain.CreateOutcomeBudgetNameTaken,
		},
		{
			name:   "campaign name taken",
			result: &CampaignResult{CampaignName: "c", CampaignBudgetName: "b", CampaignBudgetID: "111"},
			err: fmt.Errorf("google-ads performance max campaign creation failed (budget 111 created): %w",
				fmt.Errorf("campaigns:mutate: %w",
					&apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"DUPLICATE_CAMPAIGN_NAME"}})),
			want: domain.CreateOutcomeCampaignNameTaken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CreateOutcome(tc.result, tc.err)
			if !ok || got != tc.want {
				t.Errorf("CreateOutcome through two %%w wraps = (%q, %v), want (%q, true)", got, ok, tc.want)
			}
		})
	}
}

// TestCreateOutcome_OversizeErrorBodyDegradesToATrueClaim pins the ID-LESS arm's handling of a
// codeless apiError — doRequest builds one whenever parseErrorCodes finds no GoogleAdsFailure
// detail, of which an over-maxResponseBytes response (client.go) is only the clearest case. The
// arm must degrade to a weaker claim that is still true, never to a confident wrong one — here the
// budget create was sent and its outcome is unknown, which is exactly what CreateOutcomeUnconfirmed
// says.
//
// It pins THIS arm and no other. The budget arm's handling of the same shape is pinned by
// TestCreateOutcome_ClassifiesEveryPartialShape's own codeless-4xx row, because one call site's
// test does not pin another's — which is how the budget arm shipped unguarded in the first place.
func TestCreateOutcome_OversizeErrorBodyDegradesToATrueClaim(t *testing.T) {
	namePartial := &CampaignResult{CampaignName: "c", CampaignBudgetName: "b"}
	noCodes := &apiError{StatusCode: 400, Method: http.MethodPost, Body: "response exceeds 1048576 bytes"}

	got, ok := CreateOutcome(namePartial, noCodes)
	if !ok || got != domain.CreateOutcomeUnconfirmed {
		t.Errorf("CreateOutcome without error codes = (%q, %v), want (%q, true)",
			got, ok, domain.CreateOutcomeUnconfirmed)
	}
}

// TestAPIError_CarriesUpstreamStatus pins the carrier this client was missing. Without it the
// status branch of service.DispatchFailureMessage is unreachable for google-ads, whatever the
// platform answered.
func TestAPIError_CarriesUpstreamStatus(t *testing.T) {
	var ae error = &apiError{StatusCode: 400, Method: http.MethodPost, Path: "/campaigns:mutate"}

	status, ok := domain.UpstreamStatus(fmt.Errorf("google-ads dispatch: %w", ae))
	if !ok || status != 400 {
		t.Fatalf("UpstreamStatus through a wrap = (%d, %v), want (400, true)", status, ok)
	}

	// The status, and nothing else about the response.
	var carrier domain.UpstreamStatusCarrier
	if !errors.As(ae, &carrier) {
		t.Fatal("*apiError must implement domain.UpstreamStatusCarrier")
	}
}

// TestCreateOutcome_DeclinedReuseWithAPossibleWriteIsNotBudgetNameTaken pins the guard that
// keeps the budget-name-taken sentence true after budget adoption. A declined reuse returns
// BOTH errors — Google's original DUPLICATE_NAME refusal and the reason the reuse was
// declined — and one possible reason is an amount alignment Google ACCEPTED without a usable
// acknowledgement. The budget create did fail, but the EXISTING budget's amount may have
// moved, so "nothing new was created" would be false.
//
// The companion case is the one the guard must NOT catch: a declined reuse whose cause is a
// failed lookup READ. Nothing was written then, and the operator keeps the precise token.
func TestCreateOutcome_DeclinedReuseWithAPossibleWriteIsNotBudgetNameTaken(t *testing.T) {
	namePartial := &CampaignResult{CampaignName: "c", CampaignBudgetName: "b"}
	duplicate := fmt.Errorf("campaignBudgets:mutate: %w",
		&apiError{StatusCode: 400, Method: http.MethodPost, ErrorCodes: []string{"DUPLICATE_NAME"}})

	t.Run("alignment mutate may have landed", func(t *testing.T) {
		reuseErr := fmt.Errorf("aligning it failed: %w",
			&unconfirmedBudgetMutateError{err: errors.New("amount update returned an unusable response")})
		// The wrap order is production's: the DUPLICATE_NAME refusal first, the reuse
		// failure second — see the five channel files for why that order is load-bearing.
		err := fmt.Errorf("already exists (DUPLICATE_NAME): %w — and could not be safely reused: %w",
			duplicate, reuseErr)

		got, ok := CreateOutcome(namePartial, err)
		if !ok || got != domain.CreateOutcomeUnconfirmed {
			t.Errorf("CreateOutcome = (%q, %v), want (%q, true): an accepted-but-unacknowledged amount mutate means the account may have changed",
				got, ok, domain.CreateOutcomeUnconfirmed)
		}
	})

	t.Run("lookup read failed, nothing written", func(t *testing.T) {
		reuseErr := fmt.Errorf("looking up the budget failed: %w",
			&apiError{StatusCode: 500, Method: http.MethodPost})
		// The wrap order is production's: the DUPLICATE_NAME refusal first, the reuse
		// failure second — see the five channel files for why that order is load-bearing.
		err := fmt.Errorf("already exists (DUPLICATE_NAME): %w — and could not be safely reused: %w",
			duplicate, reuseErr)

		got, ok := CreateOutcome(namePartial, err)
		if !ok || got != domain.CreateOutcomeBudgetNameTaken {
			t.Errorf("CreateOutcome = (%q, %v), want (%q, true): a failed READ wrote nothing, so the precise token stands",
				got, ok, domain.CreateOutcomeBudgetNameTaken)
		}
	})
}

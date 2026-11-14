// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestValidateConversionGoalIDs(t *testing.T) {
	over := make([]string, MaxConversionGoals+1)
	for i := range over {
		over[i] = fmt.Sprintf("%d", 100+i)
	}
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"nothing", nil, ""},
		{"valid", []string{"5001", "5002"}, ""},
		{"at cap", over[:MaxConversionGoals], ""},
		{"blank", []string{""}, "entry 1 is not a Microsoft conversion goal id"},
		{"padded", []string{"1", " 2"}, "entry 2 is not"},
		{"leading zero", []string{"012"}, "entry 1 is not"},
		{"repeat", []string{"5", "5"}, "entry 2 repeats"},
		{"over cap", over, "holds 21 ids; at most 20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConversionGoalIDs(tc.ids)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var gErr *ConversionGoalError
			if !errors.As(err, &gErr) || gErr.Field != FieldConversionGoalIDs || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a ConversionGoalError containing %q", err, tc.want)
			}
		})
	}
}

// The pre-create check is ONE GetConversionGoalsByIds read naming every id and every type.
func TestCheckConversionGoals_RequestShapeAndSuccess(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"ConversionGoals":[{"Id":5002,"Status":"Active"},{"Id":"5001","Status":"Active"}],"PartialErrors":null}`
	})
	if err := c.CheckConversionGoals(context.Background(), []string{"5002", "5001"}); err != nil {
		t.Fatalf("CheckConversionGoals: %v", err)
	}
	cs := calls()
	const want = `{"ConversionGoalIds":[5002,5001],"ConversionGoalTypes":"Url Duration PagesViewedPerVisit Event AppInstall OfflineConversion InStoreTransaction"}`
	if len(cs) != 1 || cs[0].method != http.MethodPost || cs[0].path != "ConversionGoals/QueryByIds" || cs[0].body != want {
		t.Fatalf("calls = %+v, want one POST ConversionGoals/QueryByIds %s", cs, want)
	}
}

// A null entry (not retrieved) or a Paused or Deleted goal is a refusal naming the id; an answer
// that is not index-aligned or names another goal is unbelievable, not a refusal.
func TestCheckConversionGoals_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		refusal          bool
	}{
		{"missing", `{"ConversionGoals":[{"Id":5001,"Status":"Active"},null]}`, `no conversion goal "5002"`, true},
		{"paused", `{"ConversionGoals":[{"Id":5001,"Status":"Active"},{"Id":5002,"Status":"Paused"}]}`, `"5002" is Paused`, true},
		{"deleted", `{"ConversionGoals":[{"Id":5001,"Status":"Deleted"},{"Id":5002,"Status":"Active"}]}`, `"5001" is Deleted`, true},
		{"short", `{"ConversionGoals":[{"Id":5001,"Status":"Active"}]}`, "returned 1 entries for 2 ids", false},
		{"other goal", `{"ConversionGoals":[{"Id":5001,"Status":"Active"},{"Id":9,"Status":"Active"}]}`, "position 2 is not the conversion goal asked for", false},
		{"no array", `{"PartialErrors":[]}`, "no ConversionGoals array", false},
		{"not json", `[`, "not the documented shape", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, tc.body })
			err := c.CheckConversionGoals(context.Background(), []string{"5001", "5002"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			var ref *domain.PreflightRefusal
			if errors.As(err, &ref) != tc.refusal {
				t.Errorf("refusal = %v, want %v (%v)", !tc.refusal, tc.refusal, err)
			}
		})
	}
}

func TestListConversionGoals(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"ConversionGoals":[{"Id":5001,"Name":"Registration","Status":"Active","Type":"Url","Scope":"Account"}],"PartialErrors":[]}`
	})
	goals, err := c.ListConversionGoals(context.Background())
	if err != nil || len(goals) != 1 || goals[0] != (ConversionGoal{ID: "5001", Name: "Registration", Status: "Active", Type: "Url", Scope: "Account"}) {
		t.Fatalf("goals = %+v (%v)", goals, err)
	}
	if cs := calls(); len(cs) != 1 || !strings.Contains(cs[0].body, `"ConversionGoalIds":null`) {
		t.Fatalf("calls = %+v, want ids null (every goal)", cs)
	}
	for _, body := range []string{`{}`, `{"ConversionGoals":[null]}`, `{"ConversionGoals":[{"Id":1},{"Id":1}]}`, `{"ConversionGoals":[],"PartialErrors":[{"Index":0,"ErrorCode":"X","Code":1}]}`} {
		c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, body })
		if _, err := c.ListConversionGoals(context.Background()); !errors.Is(err, ErrConversionGoalsMalformed) {
			t.Errorf("%s: err = %v, want ErrConversionGoalsMalformed", body, err)
		}
	}
}

func TestValidateBiddingScheme_MaxConversions(t *testing.T) {
	s, err := validateBiddingScheme(BidSchemeMaxConversions, false, 25.5, 1)
	if err != nil || s.Type != BidSchemeMaxConversions || s.TargetCpa == nil || *s.TargetCpa != 25.5 {
		t.Fatalf("scheme = %+v (%v)", s, err)
	}
	if s, err := validateBiddingScheme(BidSchemeMaxConversions, false, 0, 1); err != nil || s.TargetCpa != nil {
		t.Fatalf("no target: scheme = %+v (%v)", s, err)
	}
	for _, tc := range []struct {
		scheme string
		cpc    bool
		target float64
		goals  int
		want   string
	}{
		{BidSchemeMaxConversions, false, 0, 0, "needs at least one conversion goal"},
		{BidSchemeMaxConversions, true, 0, 1, "cannot be combined with the MaxConversions"},
		{BidSchemeMaxConversions, false, -1, 1, "finite number greater than zero"},
		{BidSchemeMaxClicks, false, 5, 1, "only with the MaxConversions"},
	} {
		if _, err := validateBiddingScheme(tc.scheme, tc.cpc, tc.target, tc.goals); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want %q", tc, err, tc.want)
		}
	}
}

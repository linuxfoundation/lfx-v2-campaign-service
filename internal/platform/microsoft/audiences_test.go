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

func f64(v float64) *float64 { return &v }

func isAudienceRefusal(err error) bool {
	var r *domain.PreflightRefusal
	return errors.As(err, &r)
}

func TestValidateAudiences(t *testing.T) {
	over := make([]string, MaxAudienceIDs+1)
	for i := range over {
		over[i] = fmt.Sprintf("%d", 1000+i)
	}
	for _, tc := range []struct {
		name             string
		include, exclude []string
		adj              *float64
		want             string // "" = accepted
	}{
		{"nothing", nil, nil, nil, ""},
		{"valid", []string{"8394012"}, []string{"8394077"}, f64(25), ""},
		{"adjustment bounds", []string{"1"}, nil, f64(-90), ""},
		{"adjustment top", []string{"1"}, nil, f64(900), ""},
		{"blank", []string{""}, nil, nil, "audienceIds: entry 1 is not a Microsoft audience id"},
		{"padded", []string{" 1"}, nil, nil, "audienceIds: entry 1 is not"},
		{"leading zero", nil, []string{"0123"}, nil, "excludedAudienceIds: entry 1 is not"},
		{"over int64", []string{"9223372036854775808"}, nil, nil, "audienceIds: entry 1 is not"},
		{"repeat", []string{"1", "2", "1"}, nil, nil, "audienceIds: entry 3 repeats"},
		{"overlap", []string{"5"}, []string{"6", "5"}, nil, "excludedAudienceIds: entry 2 is also in audienceIds"},
		{"over cap", over, nil, nil, fmt.Sprintf("audienceIds: holds %d ids; at most %d", MaxAudienceIDs+1, MaxAudienceIDs)},
		{"adjustment without lists", nil, []string{"1"}, f64(10), "audienceBidAdjustment: applies to audienceIds"},
		{"fractional adjustment", []string{"1"}, nil, f64(12.5), "audienceBidAdjustment: must be a whole number"},
		{"adjustment too low", []string{"1"}, nil, f64(-91), "audienceBidAdjustment: must be between -90 and 900"},
		{"adjustment too high", []string{"1"}, nil, f64(901), "audienceBidAdjustment: must be between"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAudiences(tc.include, tc.exclude, tc.adj)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var aErr *AudienceError
			if !errors.As(err, &aErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an AudienceError containing %q", err, tc.want)
			}
		})
	}
}

// The pre-create check sends one GetAudiencesByIds for every id, exclusions first, typed
// RemarketingList, and accepts an all-remarketing-list answer.
func TestCheckRemarketingLists_RequestShapeAndSuccess(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[{"Id":"77","Type":"RemarketingList"},{"Id":11,"Type":"RemarketingList"},{"Id":"12","Type":"RemarketingList"}],"PartialErrors":[]}`
	})
	if err := c.CheckRemarketingLists(context.Background(), []string{"11", "12"}, []string{"77"}); err != nil {
		t.Fatalf("CheckRemarketingLists: %v", err)
	}
	cs := calls()
	const want = `{"AudienceIds":[77,11,12],"Type":"RemarketingList"}`
	if len(cs) != 1 || cs[0].method != http.MethodPost || cs[0].path != "Audiences/QueryByIds" || cs[0].body != want {
		t.Fatalf("request = %+v\nwant body %s", cs, want)
	}
	// Nothing requested sends nothing.
	c2, calls2 := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{}` })
	if err := c2.CheckRemarketingLists(context.Background(), nil, nil); err != nil || len(calls2()) != 0 {
		t.Fatalf("empty: err=%v calls=%v", err, calls2())
	}
}

// A missing list, a partial error, or another audience type is a refusal naming the field and
// id; an answer that cannot be believed, or a failed call, is a plain error.
func TestCheckRemarketingLists_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		refusal    bool
		want       string
	}{
		{"null slot", `{"Audiences":[{"Id":77,"Type":"RemarketingList"},null],"PartialErrors":[]}`, 200, true, `invalid microsoftConfig.audienceIds: Microsoft Advertising has no remarketing list "11"`},
		{"short array", `{"Audiences":[{"Id":77,"Type":"RemarketingList"}]}`, 200, true, `no remarketing list "11"`},
		{"partial error", `{"Audiences":[null,null],"PartialErrors":[{"Code":1032,"ErrorCode":"CampaignServiceAudienceIdDoesNotMatchAudienceType","Index":0}]}`, 200, true,
			`invalid microsoftConfig.excludedAudienceIds: Microsoft Advertising refused remarketing list "77" (CampaignServiceAudienceIdDoesNotMatchAudienceType)`},
		{"wrong type", `{"Audiences":[{"Id":77,"Type":"RemarketingList"},{"Id":11,"Type":"CustomerList"}]}`, 200, true, `audience "11" is a CustomerList audience, not a remarketing list`},
		{"another id", `{"Audiences":[{"Id":77,"Type":"RemarketingList"},{"Id":12,"Type":"RemarketingList"}]}`, 200, false, "could not be confirmed"},
		{"too many", `{"Audiences":[{"Id":77,"Type":"RemarketingList"},{"Id":11,"Type":"RemarketingList"},{"Id":12,"Type":"RemarketingList"}]}`, 200, false, "more audiences"},
		{"unattributable error", `{"Audiences":[{"Id":77,"Type":"RemarketingList"},{"Id":11,"Type":"RemarketingList"}],"PartialErrors":[{"Code":1,"ErrorCode":"X"}]}`, 200, false, "attributed"},
		{"not json", `nope`, 200, false, "documented shape"},
		{"no Audiences key", `{"PartialErrors":[]}`, 200, false, "no Audiences array"},
		{"null body", `null`, 200, false, "no Audiences array"},
		{"null Audiences", `{"Audiences":null}`, 200, false, "no Audiences array"},
		{"unreadable error code", `{"Audiences":[{"Id":77,"Type":"RemarketingList"},{"Id":11,"Type":"RemarketingList"}],"PartialErrors":[{"Index":1,"ErrorCode":true}]}`, 200, false, "cannot read"},
		{"500", `{}`, 500, false, "lookup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return tc.status, tc.body })
			err := c.CheckRemarketingLists(context.Background(), []string{"11"}, []string{"77"})
			if err == nil || isAudienceRefusal(err) != tc.refusal || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (refusal=%v), want refusal=%v containing %q", err, isAudienceRefusal(err), tc.refusal, tc.want)
			}
		})
	}
}

func audienceOutcomes(out []AudienceOutcome) string {
	s := make([]string, len(out))
	for i, o := range out {
		kind := "obs"
		if o.Excluded {
			kind = "exc"
		}
		s[i] = kind + ":" + o.AudienceID + "=" + o.Outcome
	}
	return strings.Join(s, ",")
}

// Exclusions go first, as NegativeAdGroupCriterion; observed lists follow as
// BiddableAdGroupCriterion with the BidMultiplier; both with CriterionType Audience.
func TestAttachAdGroupAudiences_RequestShapeAndOutcomes(t *testing.T) {
	n := 0
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		n++
		if n == 1 {
			return http.StatusOK, `{"AdGroupCriterionIds":["5001"],"NestedPartialErrors":null}`
		}
		return http.StatusOK, `{"AdGroupCriterionIds":[6001,null,null],"NestedPartialErrors":[{"Index":1,"Code":4000,"ErrorCode":"CampaignServiceAudienceInvalid","BatchErrors":null}]}`
	})
	out, err := c.AttachAdGroupAudiences(context.Background(), "444", []string{"11", "12", "13"}, []string{"77"}, 25)
	if err != nil {
		t.Fatalf("AttachAdGroupAudiences: %v", err)
	}
	if got := audienceOutcomes(out); got != "exc:77=APPLIED,obs:11=APPLIED,obs:12=FAILED,obs:13=UNCONFIRMED" {
		t.Fatalf("outcomes = %s", got)
	}
	if out[0].CriterionID != "5001" || out[1].CriterionID != "6001" || out[2].ErrorCode != "CampaignServiceAudienceInvalid" || out[3].CriterionID != "" {
		t.Errorf("details = %+v", out)
	}
	cs := calls()
	wantExc := `{"AdGroupCriterions":[{"Type":"NegativeAdGroupCriterion","AdGroupId":444,"Criterion":{"Type":"AudienceCriterion","AudienceId":77}}],"CriterionType":"Audience"}`
	wantObs := `{"AdGroupCriterions":[` +
		`{"Type":"BiddableAdGroupCriterion","AdGroupId":444,"Criterion":{"Type":"AudienceCriterion","AudienceId":11},"CriterionBid":{"Type":"BidMultiplier","Multiplier":25}},` +
		`{"Type":"BiddableAdGroupCriterion","AdGroupId":444,"Criterion":{"Type":"AudienceCriterion","AudienceId":12},"CriterionBid":{"Type":"BidMultiplier","Multiplier":25}},` +
		`{"Type":"BiddableAdGroupCriterion","AdGroupId":444,"Criterion":{"Type":"AudienceCriterion","AudienceId":13},"CriterionBid":{"Type":"BidMultiplier","Multiplier":25}}],"CriterionType":"Audience"}`
	if len(cs) != 2 || cs[0].path != "AdGroupCriterions" || cs[0].body != wantExc || cs[1].body != wantObs {
		t.Fatalf("requests = %+v", cs)
	}
}

// A call that fails is reported per list: a definite refusal FAILED, an ambiguous answer
// UNCONFIRMED, an unreadable 2xx UNCONFIRMED; the other call still runs.
func TestAttachAdGroupAudiences_CallFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"400", http.StatusBadRequest, `{"Code":1001,"ErrorCode":"InvalidRequest"}`, "exc:77=FAILED,obs:11=APPLIED"},
		{"500", http.StatusInternalServerError, `{}`, "exc:77=UNCONFIRMED,obs:11=APPLIED"},
		{"no id array", http.StatusOK, `{"NestedPartialErrors":[]}`, "exc:77=UNCONFIRMED,obs:11=APPLIED"},
		{"too many ids", http.StatusOK, `{"AdGroupCriterionIds":[1,2]}`, "exc:77=UNCONFIRMED,obs:11=APPLIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			c, _ := kwRecorder(t, func(string, string) (int, string) {
				n++
				if n == 1 {
					return tc.status, tc.body
				}
				return http.StatusOK, `{"AdGroupCriterionIds":[6001]}`
			})
			out, err := c.AttachAdGroupAudiences(context.Background(), "444", []string{"11"}, []string{"77"}, 0)
			if got := audienceOutcomes(out); got != tc.want || err == nil {
				t.Fatalf("outcomes = %s err = %v, want %s and an error", got, err, tc.want)
			}
		})
	}
}

// A context that ended sends nothing; every list is FAILED, NOT_SENT. A bad ad group id is
// refused before any request.
func TestAttachAdGroupAudiences_NotSent(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{"AdGroupCriterionIds":[1]}` })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := c.AttachAdGroupAudiences(ctx, "444", []string{"11"}, []string{"77"}, 0)
	if err == nil || len(calls()) != 0 || audienceOutcomes(out) != "exc:77=FAILED,obs:11=FAILED" || out[0].ErrorCode != errorCodeNotSent {
		t.Fatalf("out=%+v err=%v calls=%v", out, err, calls())
	}
	if out, err := c.AttachAdGroupAudiences(context.Background(), "0x1", []string{"11"}, nil, 0); err == nil || out != nil {
		t.Fatalf("bad ad group id: out=%v err=%v", out, err)
	}
}

// A list Microsoft refuses ONLY as a duplicate of a criterion the ad group already carries (4152)
// is ALREADY_PRESENT; a duplicate travelling with another error, or a numeric 4152 under another
// symbolic name, stays FAILED.
func TestAttachAdGroupAudiences_DuplicateIsAlreadyPresent(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"AdGroupCriterionIds":[null,null,null,null],"NestedPartialErrors":[` +
			`{"Index":0,"Code":4152,"ErrorCode":"CampaignServiceDuplicateAdGroupCriterion"},` +
			`{"Index":1,"Code":0,"ErrorCode":null,"BatchErrors":[{"Code":4152,"ErrorCode":null}]},` +
			`{"Index":2,"Code":4152,"ErrorCode":"CampaignServiceDuplicateAdGroupCriterion","BatchErrors":[{"Code":1001,"ErrorCode":"Other"}]},` +
			`{"Index":3,"Code":4152,"ErrorCode":"SomethingElse"}]}`
	})
	out, err := c.AttachAdGroupAudiences(context.Background(), "444", []string{"11", "12", "13", "14"}, nil, 0)
	if err != nil {
		t.Fatalf("AttachAdGroupAudiences: %v", err)
	}
	if got := audienceOutcomes(out); got != "obs:11=ALREADY_PRESENT,obs:12=ALREADY_PRESENT,obs:13=FAILED,obs:14=FAILED" {
		t.Fatalf("outcomes = %s", got)
	}
}

// Evidence that cannot cover every list leaves a list with an id but no error of its own
// UNCONFIRMED: a truncated outer or nested error array, or an error with no usable index. A zero
// or empty code is a default, not an error, so a padding collection does not refuse a list.
func TestAttachAdGroupAudiences_IncompleteEvidenceAndZeroCodes(t *testing.T) {
	pad := make([]string, maxAudienceErrorItems+1)
	for i := range pad {
		pad[i] = `{"Index":null,"Code":0,"ErrorCode":null}`
	}
	nested := make([]string, maxIndexedErrorItems+1)
	for i := range nested {
		nested[i] = `{"Code":0}`
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"outer truncated", `{"AdGroupCriterionIds":[6001],"NestedPartialErrors":[` + strings.Join(pad, ",") + `]}`, "obs:11=UNCONFIRMED"},
		{"nested truncated", `{"AdGroupCriterionIds":[6001],"NestedPartialErrors":[{"Index":null,"Code":0,"BatchErrors":[` + strings.Join(nested, ",") + `]}]}`, "obs:11=UNCONFIRMED"},
		{"unattributable error", `{"AdGroupCriterionIds":[6001],"NestedPartialErrors":[{"Code":1001,"ErrorCode":"X"}]}`, "obs:11=UNCONFIRMED"},
		{"duplicate with truncated evidence", `{"AdGroupCriterionIds":[null],"NestedPartialErrors":[{"Index":0,"Code":4152,"ErrorCode":"CampaignServiceDuplicateAdGroupCriterion"},` + strings.Join(pad, ",") + `]}`, "obs:11=UNCONFIRMED"},
		{"duplicate with nested truncated evidence", `{"AdGroupCriterionIds":[null],"NestedPartialErrors":[{"Index":0,"Code":4152,"ErrorCode":"CampaignServiceDuplicateAdGroupCriterion"},{"Index":null,"BatchErrors":[` + strings.Join(nested, ",") + `]}]}`, "obs:11=UNCONFIRMED"},
		{"unreadable collection code", `{"AdGroupCriterionIds":[6001],"NestedPartialErrors":[{"Index":0,"ErrorCode":true}]}`, "obs:11=FAILED"},
		{"unreadable nested code", `{"AdGroupCriterionIds":[6001],"NestedPartialErrors":[{"Index":0,"Code":0,"BatchErrors":[{"ErrorCode":{"x":1}}]}]}`, "obs:11=FAILED"},
		{"unreadable symbolic beside numeric 4152", `{"AdGroupCriterionIds":[null],"NestedPartialErrors":[{"Index":0,"Code":4152,"ErrorCode":true}]}`, "obs:11=FAILED"},
		{"zero-code padding", `{"AdGroupCriterionIds":[6001],"NestedPartialErrors":[{"Index":0,"Code":0,"ErrorCode":"","BatchErrors":[{"Code":0,"ErrorCode":null}]}]}`, "obs:11=APPLIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, tc.body })
			out, err := c.AttachAdGroupAudiences(context.Background(), "444", []string{"11"}, nil, 0)
			if err != nil || audienceOutcomes(out) != tc.want {
				t.Fatalf("outcomes = %s err = %v, want %s", audienceOutcomes(out), err, tc.want)
			}
		})
	}
}

// A zero-code PartialErrors entry in the lookup is a default, not a refusal of the id it names.
func TestCheckRemarketingLists_ZeroCodeIsNotARefusal(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[{"Id":11,"Type":"RemarketingList"}],"PartialErrors":[{"Index":0,"Code":0,"ErrorCode":""}]}`
	})
	if err := c.CheckRemarketingLists(context.Background(), []string{"11"}, nil); err != nil {
		t.Fatalf("CheckRemarketingLists: %v", err)
	}
}

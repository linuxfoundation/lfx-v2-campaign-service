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
)

func TestValidateSharedNegativeKeywordLists(t *testing.T) {
	over := make([]string, MaxSharedNegativeKeywordLists+1)
	for i := range over {
		over[i] = fmt.Sprintf("%d", 100+i)
	}
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"nothing", nil, ""},
		{"valid", []string{"4001", "4002"}, ""},
		{"at cap", over[:MaxSharedNegativeKeywordLists], ""},
		{"blank", []string{""}, "entry 1 is not a Microsoft negative keyword list id"},
		{"padded", []string{"1", " 2"}, "entry 2 is not"},
		{"leading zero", []string{"012"}, "entry 1 is not"},
		{"repeat", []string{"5", "5"}, "entry 2 repeats"},
		{"over cap", over, "holds 21 ids; a campaign can be associated with at most 20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSharedNegativeKeywordLists(tc.ids)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var sErr *SharedListError
			if !errors.As(err, &sErr) || sErr.Field != FieldSharedNegativeKeywordListIDs || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a SharedListError containing %q", err, tc.want)
			}
		})
	}
}

// The pre-create check is ONE GetSharedEntities read of the account's negative keyword lists.
func TestCheckSharedNegativeKeywordLists_RequestShapeAndSuccess(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"SharedEntities":[{"Id":4001,"Name":"Jobs","Type":"NegativeKeywordList"},{"Id":"4002","Type":"NegativeKeywordList"},{"Id":9,"Type":"NegativeKeywordList"}]}`
	})
	if err := c.CheckSharedNegativeKeywordLists(context.Background(), []string{"4002", "4001"}); err != nil {
		t.Fatalf("CheckSharedNegativeKeywordLists: %v", err)
	}
	cs := calls()
	const want = `{"SharedEntityType":"NegativeKeywordList","SharedEntityScope":"Account"}`
	if len(cs) != 1 || cs[0].method != http.MethodPost || cs[0].path != "SharedEntities/Query" || cs[0].body != want {
		t.Fatalf("request = %+v\nwant body %s", cs, want)
	}
}

func TestCheckSharedNegativeKeywordLists_NothingRequestedSendsNothing(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{}` })
	if err := c.CheckSharedNegativeKeywordLists(context.Background(), nil); err != nil || len(calls()) != 0 {
		t.Fatalf("err = %v, calls = %v", err, calls())
	}
}

// A missing or wrong-typed list is a refusal naming the field and id; an answer that cannot be
// believed is a plain error, not a refusal.
func TestCheckSharedNegativeKeywordLists_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refusal    bool
		want       string
	}{
		{"missing", `{"SharedEntities":[{"Id":1,"Type":"NegativeKeywordList"}]}`, true, `no negative keyword list "4001"`},
		{"wrong type", `{"SharedEntities":[{"Id":4001,"Type":"PlacementExclusionList"}]}`, true, "is a PlacementExclusionList, not a negative keyword list"},
		{"no array", `{}`, false, "carries no SharedEntities array"},
		{"null array", `{"SharedEntities":null}`, false, "carries no SharedEntities array"},
		{"null entry", `{"SharedEntities":[null]}`, false, "position 1 is empty"},
		{"bad id", `{"SharedEntities":[{"Id":0,"Type":"NegativeKeywordList"}]}`, false, "position 1 has no usable id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, tc.body })
			err := c.CheckSharedNegativeKeywordLists(context.Background(), []string{"4001"})
			if err == nil || isAudienceRefusal(err) != tc.refusal || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (refusal %v), want refusal=%v containing %q", err, isAudienceRefusal(err), tc.refusal, tc.want)
			}
		})
	}
}

func sharedOutcomes(out []SharedListOutcome) string {
	var s []string
	for _, o := range out {
		s = append(s, o.ListID+"="+o.Outcome+":"+o.ErrorCode)
	}
	return strings.Join(s, ",")
}

// One SetSharedEntityAssociations call, in request order, scoped to the account.
func TestAttachCampaignSharedNegativeKeywordLists_RequestShapeAndApplied(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{"PartialErrors":null}` })
	out, err := c.AttachCampaignSharedNegativeKeywordLists(context.Background(), "321", []string{"4001", "4002"})
	if err != nil || sharedOutcomes(out) != "4001=APPLIED:,4002=APPLIED:" {
		t.Fatalf("got %s, %v", sharedOutcomes(out), err)
	}
	cs := calls()
	const want = `{"Associations":[{"EntityId":321,"EntityType":"Campaign","SharedEntityId":4001,"SharedEntityType":"NegativeKeywordList"},` +
		`{"EntityId":321,"EntityType":"Campaign","SharedEntityId":4002,"SharedEntityType":"NegativeKeywordList"}],"SharedEntityScope":"Account"}`
	if len(cs) != 1 || cs[0].path != "SharedEntityAssociations/Set" || cs[0].body != want {
		t.Fatalf("request = %+v\nwant body %s", cs, want)
	}
}

// Positional attribution, and the rules for evidence that cannot cover every list.
func TestAttachCampaignSharedNegativeKeywordLists_Outcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
		err    bool
	}{
		{"one refused", http.StatusOK, `{"PartialErrors":[{"Index":1,"Code":1234,"ErrorCode":"CampaignServiceSharedEntityInvalid"}]}`, "4001=APPLIED:,4002=FAILED:CampaignServiceSharedEntityInvalid", false},
		{"zero-code placeholder", http.StatusOK, `{"PartialErrors":[{"Index":0,"Code":null,"ErrorCode":null}]}`, "4001=APPLIED:,4002=APPLIED:", false},
		{"unattributable", http.StatusOK, `{"PartialErrors":[{"Code":1234,"ErrorCode":"X"}]}`, "4001=UNCONFIRMED:,4002=UNCONFIRMED:", false},
		{"out of range index", http.StatusOK, `{"PartialErrors":[{"Index":7,"ErrorCode":"X"}]}`, "4001=UNCONFIRMED:,4002=UNCONFIRMED:", false},
		{"unreadable 2xx", http.StatusOK, `not json`, "4001=UNCONFIRMED:,4002=UNCONFIRMED:", true},
		{"definite refusal", http.StatusBadRequest, `{"Code":1001,"ErrorCode":"InvalidRequest"}`, "4001=FAILED:,4002=FAILED:", true},
		{"ambiguous 500", http.StatusInternalServerError, `{}`, "4001=UNCONFIRMED:,4002=UNCONFIRMED:", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return tc.status, tc.body })
			out, err := c.AttachCampaignSharedNegativeKeywordLists(context.Background(), "321", []string{"4001", "4002"})
			if got := sharedOutcomes(out); got != tc.want || (err != nil) != tc.err {
				t.Fatalf("got %s (err %v), want %s (err %v)", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestAttachCampaignSharedNegativeKeywordLists_ContextEndedSendsNothing(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{}` })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := c.AttachCampaignSharedNegativeKeywordLists(ctx, "321", []string{"4001"})
	if err == nil || sharedOutcomes(out) != "4001=FAILED:NOT_SENT" || len(calls()) != 0 {
		t.Fatalf("got %s, %v, calls %v", sharedOutcomes(out), err, calls())
	}
}

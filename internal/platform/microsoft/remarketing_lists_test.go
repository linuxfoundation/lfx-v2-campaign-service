// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The read is ONE GetAudiencesByIds call with no ids, typed RemarketingList, and every field of
// a believable answer is mapped; an absent size or duration stays nil.
func TestListRemarketingLists_RequestShapeAndMapping(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[` +
			`{"Id":8394012,"Type":"RemarketingList","Name":"KubeCon visitors","Description":"30d","Scope":"Account","MembershipDuration":30,"SearchSize":12000},` +
			`{"Id":"8394077","Type":"RemarketingList","Name":"Registrants"}],"PartialErrors":[]}`
	})
	rows, err := c.ListRemarketingLists(context.Background())
	if err != nil {
		t.Fatalf("ListRemarketingLists: %v", err)
	}
	cs := calls()
	const want = `{"AudienceIds":null,"Type":"RemarketingList"}`
	if len(cs) != 1 || cs[0].method != http.MethodPost || cs[0].path != "Audiences/QueryByIds" || cs[0].body != want {
		t.Fatalf("request = %+v\nwant body %s", cs, want)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	a, b := rows[0], rows[1]
	if a.ID != "8394012" || a.Name != "KubeCon visitors" || a.Description != "30d" || a.Scope != "Account" ||
		a.MembershipDuration == nil || *a.MembershipDuration != 30 || a.SearchSize == nil || *a.SearchSize != 12000 {
		t.Errorf("row 1 = %+v", a)
	}
	if b.ID != "8394077" || b.MembershipDuration != nil || b.SearchSize != nil {
		t.Errorf("row 2 = %+v", b)
	}
}

// An account with no list is an empty, non-nil answer.
func TestListRemarketingLists_EmptyAccount(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{"Audiences":[]}` })
	rows, err := c.ListRemarketingLists(context.Background())
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("got (%v, %v), want an empty, non-nil list", rows, err)
	}
}

// Every answer this client cannot believe is refused whole, with no rows.
func TestListRemarketingLists_RefusesAnUnbelievableAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"no array":       `{}`,
		"null array":     `{"Audiences":null}`,
		"null entry":     `{"Audiences":[null]}`,
		"no id":          `{"Audiences":[{"Type":"RemarketingList","Name":"x"}]}`,
		"bad id":         `{"Audiences":[{"Id":"012","Type":"RemarketingList"}]}`,
		"repeated id":    `{"Audiences":[{"Id":1,"Type":"RemarketingList"},{"Id":1,"Type":"RemarketingList"}]}`,
		"other type":     `{"Audiences":[{"Id":1,"Type":"InMarket"}]}`,
		"partial error":  `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[{"Code":4119,"ErrorCode":"X","Index":0}]}`,
		"not json shape": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, body })
			rows, err := c.ListRemarketingLists(context.Background())
			if !errors.Is(err, ErrRemarketingListsMalformed) || rows != nil {
				t.Fatalf("got (%v, %v), want ErrRemarketingListsMalformed and no rows", rows, err)
			}
		})
	}
}

// A zero-code placeholder in PartialErrors is not an error.
func TestListRemarketingLists_DefaultCodeIsNotAnError(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[{"Code":0,"ErrorCode":null}]}`
	})
	if rows, err := c.ListRemarketingLists(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("got (%v, %v)", rows, err)
	}
}

// An upstream failure is an error naming the read.
func TestListRemarketingLists_UpstreamFailure(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusBadRequest, `{"Code":1001}` })
	if _, err := c.ListRemarketingLists(context.Background()); err == nil || !strings.Contains(err.Error(), "remarketing list read failed") {
		t.Fatalf("err = %v", err)
	}
}

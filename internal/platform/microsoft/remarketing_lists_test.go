// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
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
		"no array":          `{}`,
		"null array":        `{"Audiences":null}`,
		"null entry":        `{"Audiences":[null]}`,
		"no id":             `{"Audiences":[{"Type":"RemarketingList","Name":"x"}]}`,
		"bad id":            `{"Audiences":[{"Id":"012","Type":"RemarketingList"}]}`,
		"repeated id":       `{"Audiences":[{"Id":1,"Type":"RemarketingList"},{"Id":1,"Type":"RemarketingList"}]}`,
		"other type":        `{"Audiences":[{"Id":1,"Type":"InMarket"}]}`,
		"partial error":     `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[{"Code":4119,"ErrorCode":"X","Index":0}]}`,
		"not json shape":    `[]`,
		"bad search size":   `{"Audiences":[{"Id":1,"Type":"RemarketingList","SearchSize":"12k"}]}`,
		"fractional size":   `{"Audiences":[{"Id":1,"Type":"RemarketingList","SearchSize":1.5}]}`,
		"negative size":     `{"Audiences":[{"Id":1,"Type":"RemarketingList","SearchSize":"-3"}]}`,
		"bad duration":      `{"Audiences":[{"Id":1,"Type":"RemarketingList","MembershipDuration":"thirty"}]}`,
		"duration overflow": `{"Audiences":[{"Id":1,"Type":"RemarketingList","MembershipDuration":99999999999}]}`,
		// One past math.MaxInt32, bare and quoted: MembershipDuration is an int, so the 32-bit
		// parse refuses it rather than converting a wider value to int.
		"duration max int32+1":        `{"Audiences":[{"Id":1,"Type":"RemarketingList","MembershipDuration":2147483648}]}`,
		"quoted duration max int32+1": `{"Audiences":[{"Id":1,"Type":"RemarketingList","MembershipDuration":"2147483648"}]}`,
		"size max int64+1":            `{"Audiences":[{"Id":1,"Type":"RemarketingList","SearchSize":"9223372036854775808"}]}`,
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

// The largest values each field's type holds are accepted unchanged: MembershipDuration
// math.MaxInt32 and SearchSize math.MaxInt64.
func TestListRemarketingLists_AcceptsTheLargestInRangeValues(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[{"Id":1,"Type":"RemarketingList","MembershipDuration":2147483647,"SearchSize":"9223372036854775807"}]}`
	})
	rows, err := c.ListRemarketingLists(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("got (%+v, %v)", rows, err)
	}
	if r := rows[0]; r.MembershipDuration == nil || *r.MembershipDuration != math.MaxInt32 || r.SearchSize == nil || *r.SearchSize != math.MaxInt64 {
		t.Errorf("row = %+v", r)
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

// Microsoft's REST JSON sends a long as a quoted string: SearchSize "12000" (and a quoted
// MembershipDuration) is read like the bare number, and null is absent.
func TestListRemarketingLists_QuotedAndNumericLongs(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[` +
			`{"Id":1,"Type":"RemarketingList","MembershipDuration":"30","SearchSize":"12000"},` +
			`{"Id":2,"Type":"RemarketingList","MembershipDuration":540,"SearchSize":9007199254740993},` +
			`{"Id":3,"Type":"RemarketingList","MembershipDuration":null,"SearchSize":null}]}`
	})
	rows, err := c.ListRemarketingLists(context.Background())
	if err != nil || len(rows) != 3 {
		t.Fatalf("got (%+v, %v)", rows, err)
	}
	if r := rows[0]; r.SearchSize == nil || *r.SearchSize != 12000 || r.MembershipDuration == nil || *r.MembershipDuration != 30 {
		t.Errorf("quoted row = %+v", r)
	}
	if r := rows[1]; r.SearchSize == nil || *r.SearchSize != 9007199254740993 || r.MembershipDuration == nil || *r.MembershipDuration != 540 {
		t.Errorf("numeric row = %+v", r)
	}
	if r := rows[2]; r.SearchSize != nil || r.MembershipDuration != nil {
		t.Errorf("null row = %+v", r)
	}
}

// The row cap stops decoding: maxRemarketingListRows lists are accepted, and one more is refused
// as soon as that entry begins — before the decoder reaches the rest of the (already buffered)
// body, here garbage that a whole-body decode would reject as "not the documented shape".
func TestListRemarketingLists_RowCapStopsDecoding(t *testing.T) {
	rows := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `{"Id":` + strconv.Itoa(i+1) + `,"Type":"RemarketingList"}`
		}
		return strings.Join(parts, ",")
	}
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[` + rows(maxRemarketingListRows) + `],"PartialErrors":[]}`
	})
	if got, err := c.ListRemarketingLists(context.Background()); err != nil || len(got) != maxRemarketingListRows {
		t.Fatalf("at the cap: %d rows, %v", len(got), err)
	}
	for name, tail := range map[string]string{
		"well formed":      `]}`,
		"garbage past cap": `, THIS IS NOT JSON`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"Audiences":[` + rows(maxRemarketingListRows+1) + tail
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, body })
			got, err := c.ListRemarketingLists(context.Background())
			if !errors.Is(err, ErrRemarketingListsMalformed) || got != nil || !strings.Contains(err.Error(), "more than 5000 lists") {
				t.Fatalf("got (%d rows, %v), want the row-cap refusal", len(got), err)
			}
		})
	}
}

// A PartialErrors entry that names no outcome, a PartialErrors key repeated under another case
// (the last, empty one would otherwise hide the failure), or a row declaring Id twice does not
// establish a complete read, so the list is refused whole.
func TestListRemarketingLists_RefusesUnreadableOrDuplicatedPartialErrors(t *testing.T) {
	for name, body := range map[string]string{
		"empty entry":            `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[{}]}`,
		"null code entry":        `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[{"Code":null}]}`,
		"PartialErrors repeated": `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[{"Code":4119}],"partialerrors":[]}`,
		"Id repeated":            `{"Audiences":[{"Id":1,"ID":2,"Type":"RemarketingList"}],"PartialErrors":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, body })
			rows, err := c.ListRemarketingLists(context.Background())
			if !errors.Is(err, ErrRemarketingListsMalformed) || rows != nil {
				t.Fatalf("got (%v, %v), want ErrRemarketingListsMalformed and no rows", rows, err)
			}
		})
	}
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Audiences":[{"Id":1,"Type":"RemarketingList"}],"PartialErrors":[null]}`
	})
	if rows, err := c.ListRemarketingLists(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("null padding: got (%v, %v)", rows, err)
	}
}

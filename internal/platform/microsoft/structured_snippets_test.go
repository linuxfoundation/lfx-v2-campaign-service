// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func goodSnippets() []StructuredSnippet {
	return []StructuredSnippet{
		{Header: " courses ", Values: []string{"Kubernetes", " Observability ", "Security"}},
		{Header: "DEGREE  PROGRAMS", Values: []string{"CKA", "CKAD", "CKS", "KCNA"}},
	}
}

func TestValidateStructuredSnippets_AcceptsNormalizesAndTrims(t *testing.T) {
	out, err := ValidateStructuredSnippets(goodSnippets())
	if err != nil {
		t.Fatalf("ValidateStructuredSnippets: %v", err)
	}
	if out[0].Header != "Courses" || out[1].Header != "Degree programs" || out[0].Values[1] != "Observability" {
		t.Errorf("out = %+v", out)
	}
	if o, err := ValidateStructuredSnippets(nil); err != nil || o != nil {
		t.Errorf("nothing requested is valid and empty, got %v %v", o, err)
	}
	ten := make([]string, MaxSnippetValues)
	for i := range ten {
		ten[i] = fmt.Sprintf("v%d", i)
	}
	edge := []StructuredSnippet{
		{Header: "Types", Values: []string{strings.Repeat("a", 25), strings.Repeat("字", 12), "c"}},
		{Header: "Shows", Values: ten},
	}
	if _, err := ValidateStructuredSnippets(edge); err != nil {
		t.Errorf("boundary values must pass: %v", err)
	}
	all := make([]StructuredSnippet, 0, MaxStructuredSnippets)
	for _, h := range structuredSnippetHeaders[:MaxStructuredSnippets] {
		all = append(all, StructuredSnippet{Header: h, Values: []string{"a", "b", "c"}})
	}
	if _, err := ValidateStructuredSnippets(all); err != nil {
		t.Errorf("%d snippets must pass: %v", MaxStructuredSnippets, err)
	}
}

func TestValidateStructuredSnippets_Refusals(t *testing.T) {
	three := []string{"a", "b", "c"}
	tooMany := make([]StructuredSnippet, 0, MaxStructuredSnippets+1)
	for _, h := range structuredSnippetHeaders[:MaxStructuredSnippets+1] {
		tooMany = append(tooMany, StructuredSnippet{Header: h, Values: three})
	}
	for name, tc := range map[string]struct {
		in   []StructuredSnippet
		want string
	}{
		"too many":        {tooMany, "at most 10 structured snippets"},
		"blank header":    {[]StructuredSnippet{{Header: " ", Values: three}}, "[0].header is empty"},
		"unknown header":  {[]StructuredSnippet{{Header: "Speakers", Values: three}}, "[0].header must be one of Amenities, Brands"},
		"control header":  {[]StructuredSnippet{{Header: "Courses\n", Values: three}}, "[0].header contains a control character"},
		"repeated header": {[]StructuredSnippet{{Header: "Courses", Values: three}, {Header: "courses", Values: three}}, "[1].header repeats"},
		"two values":      {[]StructuredSnippet{{Header: "Courses", Values: []string{"a", "b"}}}, "[0].values must hold between 3 and 10 values, got 2"},
		"eleven values":   {[]StructuredSnippet{{Header: "Courses", Values: strings.Split("a b c d e f g h i j k", " ")}}, "got 11"},
		"blank value":     {[]StructuredSnippet{{Header: "Courses", Values: []string{"a", " ", "c"}}}, "[0].values[1] is empty"},
		"long value":      {[]StructuredSnippet{{Header: "Courses", Values: []string{"a", "b", strings.Repeat("c", 26)}}}, "[0].values[2] is 26 characters; the limit is 25"},
		"long wide value": {[]StructuredSnippet{{Header: "Courses", Values: []string{"a", "b", strings.Repeat("字", 13)}}}, "the limit is 12"},
		"repeated value":  {[]StructuredSnippet{{Header: "Courses", Values: []string{"CKA", "b", "cka"}}}, "[0].values[2] repeats"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateStructuredSnippets(tc.in)
			var te *TextExtensionError
			if !errors.As(err, &te) || te.Field != FieldStructuredSnippets || !strings.Contains(te.Msg, tc.want) {
				t.Fatalf("err = %v, want a structuredSnippets refusal containing %q", err, tc.want)
			}
		})
	}
}

// One association read for the type, then one create and one association per snippet, with the
// header in Microsoft's spelling and the values trimmed.
func TestAttachCampaignStructuredSnippets_Success(t *testing.T) {
	api := &textExtAPI{}
	c, calls := textExtRecorder(t, api)
	r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets())
	if err != nil || r.Degraded() {
		t.Fatalf("got %+v, %v", r, err)
	}
	if strings.Join(r.StructuredSnippetAdExtensionIDs, ",") != "9001,9002" {
		t.Fatalf("ids = %v", r.StructuredSnippetAdExtensionIDs)
	}
	got := calls()
	want := "AdExtensionsAssociations/Query,AdExtensions,AdExtensionsAssociations/Set,AdExtensions,AdExtensionsAssociations/Set"
	if strings.Join(paths(got), ",") != want {
		t.Fatalf("calls = %v", paths(got))
	}
	if !strings.Contains(got[0].body, `"AdExtensionType":"StructuredSnippetAdExtension"`) {
		t.Errorf("lookup = %s", got[0].body)
	}
	const ext0 = `"AdExtensions":[{"Type":"StructuredSnippetAdExtension","Header":"Courses","Values":["Kubernetes","Observability","Security"]}]`
	if !strings.Contains(got[1].body, ext0) {
		t.Errorf("snippet 0 body = %s\nwant %s", got[1].body, ext0)
	}
}

// A campaign that already has the requested structured snippets gets none added (a retry never
// doubles them), and that is a clean outcome: what was asked for is there.
func TestAttachCampaignStructuredSnippets_SkipsWhenRequestedAlreadyAssociated(t *testing.T) {
	const assocs = `[` +
		`{"AdExtension":{"Id":4441,"Type":"StructuredSnippetAdExtension","Header":"Courses","Values":["kubernetes","Observability","Security"]},"AssociationType":"Campaign","EntityId":321},` +
		`{"AdExtension":{"Id":4442,"Type":"StructuredSnippetAdExtension","Header":"Degree programs","Values":["CKA","CKAD","CKS","KCNA","LFCS"]},"AssociationType":"Campaign","EntityId":321}]`
	api := &textExtAPI{
		existing:       map[string]string{"StructuredSnippetAdExtension": "4441"},
		existingFields: map[string]string{"StructuredSnippetAdExtension": assocs},
	}
	c, calls := textExtRecorder(t, api)
	r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets())
	if err != nil || r.Degraded() || len(r.StructuredSnippetAdExtensionIDs) != 0 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if strings.Join(paths(calls()), ",") != "AdExtensionsAssociations/Query" {
		t.Errorf("calls = %v", paths(calls()))
	}
	if !strings.Contains(strings.Join(r.Steps, "\n"), "Structured snippet extensions already associated with the campaign (2, e.g. 4441): none added") {
		t.Errorf("steps = %v", r.Steps)
	}
}

// A reused campaign holding a DIFFERENT structured snippet still gets none added, but the
// requested snippets are not there, so the outcome is degraded (Failed), not a clean create.
func TestAttachCampaignStructuredSnippets_ExistingDifferentSnippetDegrades(t *testing.T) {
	api := &textExtAPI{
		existing:       map[string]string{"StructuredSnippetAdExtension": "4441"},
		existingFields: map[string]string{"StructuredSnippetAdExtension": `"Header":"Types","Values":["A","B","C"]`},
	}
	c, calls := textExtRecorder(t, api)
	r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets())
	if err != nil || !r.Degraded() || r.Failed != 2 || r.Unconfirmed != 0 || len(r.StructuredSnippetAdExtensionIDs) != 0 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if strings.Join(paths(calls()), ",") != "AdExtensionsAssociations/Query" {
		t.Errorf("a different existing snippet must still add none: calls = %v", paths(calls()))
	}
	if !strings.Contains(strings.Join(r.Steps, "\n"), "Structured snippet extension 0 (Courses) NOT attached") {
		t.Errorf("steps = %v", r.Steps)
	}
}

// The same header with a missing value is not the requested snippet either.
func TestAttachCampaignStructuredSnippets_ExistingSameHeaderOtherValuesDegrades(t *testing.T) {
	api := &textExtAPI{
		existing:       map[string]string{"StructuredSnippetAdExtension": "4441"},
		existingFields: map[string]string{"StructuredSnippetAdExtension": `"Header":"Courses","Values":["Kubernetes","Observability","Linux"]`},
	}
	c, _ := textExtRecorder(t, api)
	r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets()[:1])
	if err != nil || !r.Degraded() || r.Failed != 1 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

// An existing snippet whose contents Microsoft did not report may be the requested one, so the
// outcome is UNCONFIRMED (degraded) rather than success or a definite failure.
func TestAttachCampaignStructuredSnippets_ExistingUnreadableIsUnconfirmed(t *testing.T) {
	api := &textExtAPI{existing: map[string]string{"StructuredSnippetAdExtension": "4441"}}
	c, calls := textExtRecorder(t, api)
	r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets())
	if err != nil || !r.Degraded() || r.Unconfirmed != 2 || r.Failed != 0 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if strings.Join(paths(calls()), ",") != "AdExtensionsAssociations/Query" {
		t.Errorf("calls = %v", paths(calls()))
	}
}

// A refused create is counted FAILED and an ambiguous one UNCONFIRMED; the result is degraded and
// the other snippet still lands.
func TestAttachCampaignStructuredSnippets_FailuresDegrade(t *testing.T) {
	n := 0
	api := &textExtAPI{override: func(p, _ string) (int, string, bool) {
		if p != "AdExtensions" {
			return 0, "", false
		}
		n++
		if n == 1 {
			return 400, `{"Code":1001,"ErrorCode":"InvalidRequest"}`, true
		}
		return 0, "", false
	}}
	c, _ := textExtRecorder(t, api)
	r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets())
	if err != nil || !r.Degraded() || r.Failed != 1 || len(r.StructuredSnippetAdExtensionIDs) != 1 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestAttachCampaignStructuredSnippets_RefusesBeforeAnyRequest(t *testing.T) {
	c, calls := textExtRecorder(t, &textExtAPI{})
	if _, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", []StructuredSnippet{{Header: "Speakers", Values: []string{"a", "b", "c"}}}); err == nil {
		t.Fatal("an unknown header must be refused")
	}
	if _, err := c.AttachCampaignStructuredSnippets(context.Background(), "x", goodSnippets()); err == nil {
		t.Fatal("a bad campaign id must be refused")
	}
	if len(calls()) != 0 {
		t.Errorf("calls = %v, want none", calls())
	}
}

// A recognized header whose Values are absent, null or empty may be the requested snippet, so the
// outcome is UNCONFIRMED, not a definite failure; a different header still rules it out.
func TestAttachCampaignStructuredSnippets_ExistingHeaderWithoutValuesIsUnconfirmed(t *testing.T) {
	for name, fields := range map[string]string{
		"absent": `"Header":"Courses"`,
		"null":   `"Header":"Courses","Values":null`,
		"empty":  `"Header":"courses","Values":[]`,
	} {
		t.Run(name, func(t *testing.T) {
			api := &textExtAPI{
				existing:       map[string]string{"StructuredSnippetAdExtension": "4441"},
				existingFields: map[string]string{"StructuredSnippetAdExtension": fields},
			}
			c, _ := textExtRecorder(t, api)
			r, err := c.AttachCampaignStructuredSnippets(context.Background(), "321", goodSnippets())
			// Courses may be there (UNCONFIRMED); Degree programs is ruled out (FAILED).
			if err != nil || !r.Degraded() || r.Unconfirmed != 1 || r.Failed != 1 {
				t.Fatalf("got %+v, %v", r, err)
			}
			if !strings.Contains(strings.Join(r.Steps, "\n"), "Structured snippet extension 0 (Courses) UNCONFIRMED") {
				t.Errorf("steps = %v", r.Steps)
			}
		})
	}
}

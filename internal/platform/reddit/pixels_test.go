// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

// Response bodies follow Reddit's published v3 OpenAPI document (ListPixelsByAdAccountResponse:
// {"data":[components_schema_pixel...],"pagination":{...}}). They prove the walk's handling of
// that shape, not that Reddit sends it; pixels.go records that.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const pixelsPath = "/api/v3/ad_accounts/t2_acct/pixels"

func TestListPixels_WalksPagesAndMapsFields(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, key string, _ int) (int, string) {
		switch key {
		case pixelsPath:
			return 200, `{"data":[{"id":"t2_123456","name":" Snoo's Pixel ","business_id":"b1","created_at":"2026-01-01T00:00:00Z","automatic_matching_config":{}}],"pagination":` + s.next(pixelsPath+"?page.token=p2") + `}`
		case pixelsPath + "?page.token=p2":
			return 200, `{"data":[{"id":"a2_px"}],"pagination":{"next_url":null}}`
		}
		return 404, `{}`
	}}
	got, truncated, err := newCustomAudienceClient(t, stub).ListPixels(context.Background())
	if err != nil || truncated {
		t.Fatalf("ListPixels: truncated=%v err=%v", truncated, err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pixels, want 2: %+v", len(got), got)
	}
	if p := got[0]; p.ID != "t2_123456" || p.Name != "Snoo's Pixel" || p.BusinessID != "b1" {
		t.Errorf("first = %+v", p)
	}
	if p := got[1]; p.ID != "a2_px" || p.Name != "" || p.BusinessID != "" {
		t.Errorf("second = %+v", p)
	}
	if n := len(stub.seen()); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestListPixels_EmptyAccountIsEmptyNotNil(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) {
		return 200, `{"data":[],"pagination":{}}`
	}}
	got, truncated, err := newCustomAudienceClient(t, stub).ListPixels(context.Background())
	if err != nil || truncated || got == nil || len(got) != 0 {
		t.Fatalf("got (%v, %v, %v), want an empty non-nil list", got, truncated, err)
	}
}

func TestListPixels_StopsAtTheCapAndSaysSo(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, _ string, n int) (int, string) {
		return 200, fmt.Sprintf(`{"data":[{"id":"p%d"}],"pagination":%s}`, n, s.next(fmt.Sprintf("%s?page.token=p%d", pixelsPath, n+1)))
	}}
	got, truncated, err := newCustomAudienceClient(t, stub).ListPixels(context.Background())
	if err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v, want truncated", truncated, err)
	}
	if len(got) != pixelMaxPages || len(stub.seen()) != pixelMaxPages {
		t.Errorf("got %d pixels over %d requests, want %d of each", len(got), len(stub.seen()), pixelMaxPages)
	}
}

func TestListPixels_RefusesWhatItCannotBelieve(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"data absent", `{"pagination":{}}`},
		{"data null", `{"data":null}`},
		{"data not a list", `{"data":{"id":"t2_1"}}`},
		{"blank id", `{"data":[{"id":""}]}`},
		{"numeric id", `{"data":[{"id":123}]}`},
		{"path in id", `{"data":[{"id":"t2_1/../2"}]}`},
		{"too long", `{"data":[{"id":"` + strings.Repeat("p", 65) + `"}]}`},
		{"duplicate", `{"data":[{"id":"t2_1"},{"id":"t2_1"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, tc.body }}
			got, _, err := newCustomAudienceClient(t, stub).ListPixels(context.Background())
			if got != nil || !errors.Is(err, ErrPixelsMalformed) {
				t.Fatalf("got (%v, %v), want nil and ErrPixelsMalformed", got, err)
			}
		})
	}
}

func TestListPixels_UpstreamErrorIsAnError(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return http.StatusForbidden, `{}` }}
	got, truncated, err := newCustomAudienceClient(t, stub).ListPixels(context.Background())
	if got != nil || truncated || err == nil {
		t.Fatalf("got (%v, %v, %v), want an error and nothing partial", got, truncated, err)
	}
}

func TestListPixels_RefusesAnUnusableAccountWithoutCalling(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, `{"data":[]}` }}
	c := newCustomAudienceClient(t, stub)
	c.account.AccountID = "t2/../x"
	if _, _, err := c.ListPixels(context.Background()); err == nil {
		t.Fatal("an unusable account id must be refused")
	}
	if n := len(stub.seen()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

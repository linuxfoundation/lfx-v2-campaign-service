// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// monitorFailureClass reads only an error's type and status, and maps every shape a per-campaign
// report read can fail with onto the closed set.
func TestMonitorFailureClass_Mapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want model.MonitorFailureClass
	}{
		{"401", &apiError{StatusCode: http.StatusUnauthorized}, model.MonitorFailureAuth},
		{"403", fmt.Errorf("page 0: %w", &apiError{StatusCode: http.StatusForbidden}), model.MonitorFailureAuth},
		{"refused token", fmt.Errorf("%w: reddit token refresh failed: HTTP 400", ErrCredentialRejected), model.MonitorFailureAuth},
		{"429", &apiError{StatusCode: http.StatusTooManyRequests}, model.MonitorFailureThrottle},
		{"500", &apiError{StatusCode: http.StatusInternalServerError}, model.MonitorFailureUpstream5xx},
		{"503", &apiError{StatusCode: http.StatusServiceUnavailable}, model.MonitorFailureUpstream5xx},
		{"token endpoint down", fmt.Errorf("%w: HTTP 502", errTokenEndpointUnavailable), model.MonitorFailureUpstream5xx},
		{"undecodable", fmt.Errorf("%w: no metrics", errMonitorReportUndecodable), model.MonitorFailureDecode},
		{"page cap", fmt.Errorf("walk: %w", errPageCapReached), model.MonitorFailureDecode},
		{"400", &apiError{StatusCode: http.StatusBadRequest}, model.MonitorFailureOther},
		{"404", &apiError{StatusCode: http.StatusNotFound}, model.MonitorFailureOther},
		{"malformed 2xx envelope", fmt.Errorf("page 1: %w", &transportError{Err: fmt.Errorf("%w: unexpected end of JSON input", errUndecodable2xxResponse)}), model.MonitorFailureDecode},
		{"2xx body unreadable", &transportError{Err: fmt.Errorf("read 2xx response body: %w", errors.New("unexpected EOF"))}, model.MonitorFailureOther},
		{"transport", &transportError{Err: errors.New("connection reset")}, model.MonitorFailureOther},
		{"deadline", context.DeadlineExceeded, model.MonitorFailureOther},
		{"plain", errors.New("something"), model.MonitorFailureOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := monitorFailureClass(tc.err); got != tc.want {
				t.Errorf("monitorFailureClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// Each failed campaign carries the class of its own failure end to end, and a healthy sibling
// carries none.
func TestListAccountCampaigns_ClassifiesEachFailedRow(t *testing.T) {
	statuses := map[string]int{
		"camp0": http.StatusUnauthorized,
		"camp1": http.StatusTooManyRequests,
		"camp2": http.StatusBadGateway,
		"camp4": http.StatusForbidden,
	}
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(7) + `}`, report: func(id string) (int, string) {
		if s, ok := statuses[id]; ok {
			return s, `{"error":"x"}`
		}
		if id == "camp3" {
			return http.StatusOK, `{"data":"not-a-report"}`
		}
		if id == "camp5" {
			// Not JSON at all: request() fails the envelope decode before decodeReportRows runs.
			return http.StatusOK, `<html>gateway hiccup</html>`
		}
		return healthyReport(id)
	}}
	c := newMonitorClient(t, stub, fixedRedditClock())
	c.retryBaseDelay = time.Millisecond // the 429 is retried before it is reported

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	want := map[string]model.MonitorFailureClass{
		"camp0": model.MonitorFailureAuth,
		"camp1": model.MonitorFailureThrottle,
		"camp2": model.MonitorFailureUpstream5xx,
		"camp3": model.MonitorFailureDecode,
		"camp4": model.MonitorFailureAuth,
		"camp5": model.MonitorFailureDecode,
		"camp6": "",
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		w, ok := want[r.CampaignID]
		if !ok {
			t.Fatalf("unexpected row %q", r.CampaignID)
		}
		if r.FailureClass != w || r.FetchFailed != (w != "") {
			t.Errorf("%s: FetchFailed=%v FailureClass=%q, want FetchFailed=%v class %q", r.CampaignID, r.FetchFailed, r.FailureClass, w != "", w)
		}
	}
}

// A malformed campaign id is a decode failure: Reddit answered with something this client will
// not interpolate.
func TestListAccountCampaigns_MalformedCampaignIDIsDecode(t *testing.T) {
	body := `{"data":[{"id":"camp_1,campaign:id==x","name":"M","configured_status":"ACTIVE"}]}`
	c := newMonitorClient(t, &monitorStub{t: t, listBody: body, report: healthyReport}, fixedRedditClock())
	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 || rows[0].FailureClass != model.MonitorFailureDecode {
		t.Fatalf("rows = %+v, want one decode row", rows)
	}
}

// When the caller's own deadline kills the reports still in flight, the account was not read:
// the call fails rather than answering with rows the deadline made unmeasured (#220).
func TestListAccountCampaigns_CallerDeadlineFailsTheRead(t *testing.T) {
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(2) + `}`, report: healthyReport, delay: time.Second}
	c := newMonitorClient(t, stub, fixedRedditClock())
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	rows, err := c.ListAccountCampaigns(ctx, testAccount.AccountID, 7)
	if err == nil {
		t.Fatalf("ListAccountCampaigns returned %d rows and no error; want the caller's deadline to fail the read", len(rows))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

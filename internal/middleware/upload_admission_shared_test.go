// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/membudget"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestUploadAdmissionShared_ChargesTheSharedBudget pins #179's wiring at the middleware: the
// permit is taken from the budget handed in, the one the decode reservation also charges, so
// what an upload holds while its handler runs is visible in that budget's InUse and is returned
// when the handler finishes.
func TestUploadAdmissionShared_ChargesTheSharedBudget(t *testing.T) {
	budget := membudget.New(constants.UploadMemoryBudgetFallbackBytes)
	body := strings.Repeat("x", 4<<20) // 4 MiB declared, priced at 12 MiB
	want := constants.UploadAdmissionWeightFor(int64(len(body)))

	var seen int64
	h := UploadAdmissionShared(budget, constants.MaxRequestBodyBytes, constants.UploadAdmissionWeightFor, constants.UploadAdmissionWait)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			seen = budget.InUse()
			w.WriteHeader(http.StatusCreated)
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, testUploadPath, strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if seen != want {
		t.Fatalf("shared budget InUse during the handler = %d, want the upload's weight %d", seen, want)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("InUse after the handler = %d, want 0", got)
	}
}

// TestUploadAdmissionShared_AddsRetryAfterToTheHandlers503 pins the other half of the shed
// contract: when the decode reservation inside the handler cannot get budget it answers 503, and
// that 503 is the same shared budget being busy, so it must carry Retry-After like the
// middleware's own shed. A Retry-After the handler set itself is left alone, and other statuses
// get none.
func TestUploadAdmissionShared_AddsRetryAfterToTheHandlers503(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		handlerSet string
		want       string
	}{
		{"handler 503 gets Retry-After", http.StatusServiceUnavailable, "", "1"},
		{"handler's own Retry-After is kept", http.StatusServiceUnavailable, "7", "7"},
		{"a 400 gets none", http.StatusBadRequest, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := UploadAdmissionShared(membudget.New(constants.UploadMemoryBudgetFallbackBytes),
				constants.MaxRequestBodyBytes, constants.UploadAdmissionWeightFor, constants.UploadAdmissionWait)(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if tc.handlerSet != "" {
						w.Header().Set("Retry-After", tc.handlerSet)
					}
					w.WriteHeader(tc.status)
				}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, testUploadPath, strings.NewReader(`{}`)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.want {
				t.Fatalf("Retry-After = %q, want %q", got, tc.want)
			}
		})
	}
}

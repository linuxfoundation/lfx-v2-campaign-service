// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/membudget"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestDecodeReserver_SharesTheUploadBudget pins #179: the pixel reservation charges the SAME
// per-pod budget the upload middleware charges for the wire side. With a worst-case wire charge
// already held, a pixel reservation that fits a private budget of the same size but not the
// remainder of the shared one must be refused, and an admitted one must show in the shared
// budget's InUse (which the gauge reports).
func TestDecodeReserver_SharesTheUploadBudget(t *testing.T) {
	budget := membudget.New(constants.UploadMemoryBudgetFallbackBytes)
	d := NewSharedDecodeReserver(budget)

	wire, ok := budget.Acquire(context.Background(), constants.UploadAdmissionWeightBytes, time.Second)
	if !ok {
		t.Fatal("premise: one wire charge must fit")
	}
	remainder := budget.Total() - constants.UploadAdmissionWeightBytes

	start := time.Now()
	if rel, ok := d.reserve(context.Background(), remainder+1); ok {
		rel()
		t.Fatalf("a %d-byte pixel reservation was admitted with only %d left in the shared budget: the decode budget is not the upload budget",
			remainder+1, remainder)
	}
	if waited := time.Since(start); waited > 5*constants.DecodeAdmissionWait {
		t.Fatalf("the refusal waited %v; it must be bounded by DecodeAdmissionWait", waited)
	}

	rel, ok := d.reserve(context.Background(), constants.CreativeDecodePeakBytes)
	if !ok {
		t.Fatal("a worst-case pixel reservation must fit beside a worst-case wire charge")
	}
	if got, want := budget.InUse(), constants.UploadWorstCaseBytes; got != want {
		t.Fatalf("shared InUse = %d, want wire + pixels = %d", got, want)
	}
	rel()
	wire()
	if got := budget.InUse(); got != 0 {
		t.Fatalf("InUse after both releases = %d, want 0", got)
	}
}

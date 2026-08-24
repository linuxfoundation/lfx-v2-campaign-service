// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPodMemoryLimitMatchesChart keeps PodMemoryLimitBytes honest against the chart it claims to
// mirror.
//
// The admission budget is derived from a pod memory limit that lives in a DIFFERENT file, in a
// format the Go build never reads. That is the classic silently-stale duplication: someone
// raises or lowers the chart limit, nothing in Go fails, and the budget goes on being derived
// from a number that is no longer true. This test reads values.yaml and fails loudly instead.
func TestPodMemoryLimitMatchesChart(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "lfx-v2-campaign-service", "values.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative path in a test
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}

	// Scope the search to the resources.limits block: values.yaml also carries a
	// requests.memory (128Mi), and matching the first "memory:" in the file would silently
	// assert against the wrong one.
	text := string(raw)
	idx := strings.Index(text, "limits:")
	if idx < 0 {
		t.Fatalf("no limits: block in %s", path)
	}
	rest := text[idx:]
	if end := strings.Index(rest, "requests:"); end > 0 {
		rest = rest[:end]
	}

	m := regexp.MustCompile(`memory:\s*(\d+)Mi`).FindStringSubmatch(rest)
	if m == nil {
		t.Fatalf("no memory limit found in the limits block of %s", path)
	}

	var mib int64
	for _, c := range m[1] {
		mib = mib*10 + int64(c-'0')
	}
	want := mib << 20

	if PodMemoryLimitBytes != want {
		t.Errorf("PodMemoryLimitBytes = %d (%d MiB), but %s declares %sMi.\n"+
			"The admission budget is derived from this number; update the constant to match "+
			"the chart (or the chart to match the constant).",
			PodMemoryLimitBytes, PodMemoryLimitBytes>>20, path, m[1])
	}
}

// TestUploadAdmissionBudgetLeavesHeadroom asserts the PROPERTY the budget exists to hold, rather
// than restating its formula.
//
// Deriving the expectation from UploadAdmissionBudgetBytes itself would prove nothing: any value
// satisfies its own definition. The independent bound is the pod limit — whatever the budget is,
// concurrent uploads must not be able to claim the whole pod, because the runtime, the DB pool
// and every non-upload request share it.
func TestUploadAdmissionBudgetLeavesHeadroom(t *testing.T) {
	if UploadAdmissionBudgetBytes >= PodMemoryLimitBytes {
		t.Fatalf("upload budget %d MiB >= pod limit %d MiB: uploads alone could OOM the pod",
			UploadAdmissionBudgetBytes>>20, PodMemoryLimitBytes>>20)
	}

	// Uploads must leave the MAJORITY of the pod to everything else. Stated as an absolute
	// fraction of the pod limit, independent of how the budget happens to be computed.
	if UploadAdmissionBudgetBytes > PodMemoryLimitBytes/2 {
		t.Errorf("upload budget %d MiB exceeds half the %d MiB pod limit; "+
			"the runtime, DB pool and non-upload traffic share this limit",
			UploadAdmissionBudgetBytes>>20, PodMemoryLimitBytes>>20)
	}

	// The budget must admit at least one maximum-size upload, or the endpoint is bricked:
	// every upload would shed regardless of load.
	if UploadAdmissionBudgetBytes < UploadAdmissionWeightBytes {
		t.Errorf("budget %d MiB < single-upload weight %d MiB: no upload could ever be admitted",
			UploadAdmissionBudgetBytes>>20, UploadAdmissionWeightBytes>>20)
	}

	// The per-upload weight must cover the largest body the server will actually read,
	// otherwise the weight understates what one request costs and the bound does not bind.
	if UploadAdmissionWeightBytes < MaxRequestBodyBytes {
		t.Errorf("upload weight %d MiB < max request body %d MiB: the weight understates one upload",
			UploadAdmissionWeightBytes>>20, MaxRequestBodyBytes>>20)
	}
}

// TestUploadAdmissionWeightCoversTheCoexistingPeak pins the weight to memory that PROVABLY
// coexists during one upload, derived from the service's own limits rather than from the weight
// under test.
//
// The independent bound: UploadCreativeAsset calls image.Decode(bytes.NewReader(p.Bytes)), so the
// decoded slice is the decode's INPUT and is necessarily live for the decode's whole duration.
// The pixel buffer is allocated on top of it. Any weight below their sum lets two permits admit
// more bytes than the budget names — a bound that under-counts exactly when it matters.
//
// A previous revision charged 64 MiB on the reasoning that the body buffer is released before the
// decode peaks; that argument does not apply to the decoded slice, and this test is what would
// have caught it.
func TestUploadAdmissionWeightCoversTheCoexistingPeak(t *testing.T) {
	// Both figures come from the service's declared limits, NOT from UploadAdmissionWeightBytes:
	//   - the design's MaxLength on the upload's `bytes` attribute (30 MiB decoded);
	//   - maxCreativeDecodedBytes, the decode budget in internal/service (80 MiB).
	const maxDecodedPayload int64 = 30 << 20
	const maxDecodedPixels int64 = 80 << 20
	mustCoexist := maxDecodedPayload + maxDecodedPixels // ~110 MiB

	if UploadAdmissionWeightBytes < mustCoexist {
		t.Errorf("weight %d MiB is below the %d MiB that provably coexists during one upload "+
			"(%d MiB decoded payload held live as image.Decode's input + %d MiB pixel buffer): "+
			"two permits would admit ~%d MiB against a %d MiB budget",
			UploadAdmissionWeightBytes>>20, mustCoexist>>20,
			maxDecodedPayload>>20, maxDecodedPixels>>20,
			(2*mustCoexist)>>20, UploadAdmissionBudgetBytes>>20)
	}

	// The budget must still admit at least one upload at that honest weight, or the endpoint
	// sheds unconditionally.
	if UploadAdmissionBudgetBytes < UploadAdmissionWeightBytes {
		t.Errorf("budget %d MiB cannot admit a single %d MiB upload",
			UploadAdmissionBudgetBytes>>20, UploadAdmissionWeightBytes>>20)
	}
}

// TestReadTimeoutExceedsHeaderTimeout guards the relationship between the two read deadlines.
// ReadTimeout covers headers AND body, so a value at or below ReadHeaderTimeout would cut off
// legitimate uploads before the body could arrive.
func TestReadTimeoutExceedsHeaderTimeout(t *testing.T) {
	if DefaultReadTimeout <= DefaultReadHeaderTimeout {
		t.Errorf("DefaultReadTimeout (%v) must exceed DefaultReadHeaderTimeout (%v): "+
			"it covers the body as well as the headers",
			DefaultReadTimeout, DefaultReadHeaderTimeout)
	}
}

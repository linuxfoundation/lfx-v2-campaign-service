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

// TestPodMemoryLimitIsRenderedByTheDeployment closes the gap TestPodMemoryLimitMatchesChart leaves
// open, and the gap is worth stating precisely because the two tests look redundant.
//
// The test above reads values.yaml, which is the chart DEFAULT. templates/deployment.yaml renders
// the container's limits from `{{- with .Values.resources }}`, so an installation that overrides
// resources — `--set resources.limits.memory=...`, or a values file in the deploying repo —
// replaces that block wholesale. The pod then runs with a limit the Go constant never saw, and the
// admission budget silently stops being the quarter-of-the-pod it is documented to be, with no
// test failing anywhere.
//
// What this asserts is the LINK rather than a number: that the value the constant mirrors is the
// value the template actually renders into the container, and that the path it renders through is
// the overridable one. It cannot stop an operator overriding the limit — nothing in Go can — but
// it makes the override visible as the thing that must be kept in step, instead of leaving the
// budget's premise resting on a default nobody re-checks.
func TestPodMemoryLimitIsRenderedByTheDeployment(t *testing.T) {
	tmpl := filepath.Join("..", "..", "charts", "lfx-v2-campaign-service", "templates", "deployment.yaml")
	raw, err := os.ReadFile(tmpl) //nolint:gosec // fixed repo-relative path in a test
	if err != nil {
		t.Fatalf("read deployment template: %v", err)
	}
	text := string(raw)

	// The limit must reach the container through .Values.resources. If the template ever hardcoded
	// it, or renamed the key, the constant would be mirroring a value the pod no longer uses.
	if !strings.Contains(text, ".Values.resources") {
		t.Error("deployment.yaml does not render .Values.resources: PodMemoryLimitBytes claims to " +
			"mirror the pod's memory limit, but the template no longer sources it from the values " +
			"this test and TestPodMemoryLimitMatchesChart check")
	}

	// And it must be the OVERRIDABLE path — which is the whole point. A `with` guard means an
	// installation supplying its own resources block replaces the default entirely, so the
	// default this suite validates is not necessarily what runs.
	if !regexp.MustCompile(`\{\{-?\s*with\s+\.Values\.resources\s*-?\}\}`).MatchString(text) {
		t.Error("the resources block is no longer rendered through `with .Values.resources`; " +
			"re-check whether the pod limit can still be overridden independently of " +
			"PodMemoryLimitBytes, and update this test's reasoning if the mechanism changed")
	}

	// Guard the derivation itself, so the relationship the budget documents is checked against the
	// number the chart declares rather than assumed.
	if UploadMemoryBudgetFallbackBytes != PodMemoryLimitBytes/100*40 {
		t.Errorf("fallback upload budget %d MiB is not 40%% of the %d MiB pod limit; the constant's "+
			"documented derivation no longer holds",
			UploadMemoryBudgetFallbackBytes>>20, PodMemoryLimitBytes>>20)
	}
}

// TestUploadMemoryBudgetComposes asserts the property #179 exists for: ONE budget that the wire
// charge and the pixel reservation draw on together, sized so it leaves the majority of the pod to
// everything else and still admits a worst-case upload. It asserts the QUOTIENTS (effective
// concurrency), not just the terms, because each constant of the old pair was individually
// well-derived and nobody derived budget / weight.
func TestUploadMemoryBudgetComposes(t *testing.T) {
	if UploadMemoryBudgetFallbackBytes > PodMemoryLimitBytes/2 {
		t.Errorf("upload budget %d MiB exceeds half the %d MiB pod; the runtime, DB pool and "+
			"non-upload traffic share this limit", UploadMemoryBudgetFallbackBytes>>20, PodMemoryLimitBytes>>20)
	}

	// One worst-case upload holds its wire weight AND its pixel reservation at once, against the
	// same budget. If that sum exceeds the budget, every maximum-size upload sheds forever.
	if UploadWorstCaseBytes != UploadAdmissionWeightBytes+CreativeDecodePeakBytes {
		t.Fatalf("UploadWorstCaseBytes = %d, want wire weight + pixel peak = %d",
			UploadWorstCaseBytes, UploadAdmissionWeightBytes+CreativeDecodePeakBytes)
	}
	if n := UploadMemoryBudgetFallbackBytes / UploadWorstCaseBytes; n < 1 {
		t.Errorf("effective worst-case concurrency = %d: a %d MiB budget cannot hold one %d MiB "+
			"(wire %d + pixels %d) upload", n, UploadMemoryBudgetFallbackBytes>>20,
			UploadWorstCaseBytes>>20, UploadAdmissionWeightBytes>>20, CreativeDecodePeakBytes>>20)
	}

	// Undeclared (chunked) bodies are charged the full wire weight, and on a default pod they run
	// one at a time: the documented trade in UploadAdmissionWeightFor.
	if n := UploadMemoryBudgetFallbackBytes / UploadAdmissionWeightFor(-1); n != 1 {
		t.Errorf("effective undeclared concurrency = %d, want 1 (see UploadAdmissionWeightFor)", n)
	}

	// Small declared uploads must share it widely, or the budget is a throughput regression.
	if n := UploadMemoryBudgetFallbackBytes / UploadAdmissionWeightFor(64<<10); n < 16 {
		t.Errorf("effective small-upload concurrency = %d, want at least 16", n)
	}
}

// TestUploadAdmissionWeightCoversTheCoexistingPeak pins the wire weight and the pixel reservation
// to memory that PROVABLY coexists during one upload, derived from the service's own limits
// rather than from the weights under test. One upload has two peaks:
//
//   - before the pixel decode (the base64 decode): body buffer + base64 string + decoded slice;
//   - during the pixel decode: body buffer + decoded slice + pixel buffer (the string is cleared).
//
// The wire weight alone must cover the first, and wire weight + pixel reservation the second.
// The wire weight must NOT include the pixel buffer: that is the double-count #179 removed, and
// it is what made the old pair impossible to add up.
func TestUploadAdmissionWeightCoversTheCoexistingPeak(t *testing.T) {
	const (
		bodyBuffer    int64 = 42 << 20 // MaxRequestBodyBytes, the buffered body
		base64String  int64 = 40 << 20 // the design's MaxLength(41943040) string
		decodedSlice  int64 = 30 << 20 // maxCreativeStoredBytes in internal/service
		decodedPixels int64 = 80 << 20 // maxCreativeDecodedBytes in internal/service
	)
	if UploadAdmissionWeightBytes < bodyBuffer+base64String+decodedSlice {
		t.Errorf("wire weight %d MiB is below the %d MiB wire-side peak (body + base64 string + decoded slice)",
			UploadAdmissionWeightBytes>>20, (bodyBuffer+base64String+decodedSlice)>>20)
	}
	if UploadAdmissionWeightBytes+CreativeDecodePeakBytes < bodyBuffer+decodedSlice+decodedPixels {
		t.Errorf("wire weight + pixel reservation %d MiB is below the %d MiB decode-time peak",
			(UploadAdmissionWeightBytes+CreativeDecodePeakBytes)>>20, (bodyBuffer+decodedSlice+decodedPixels)>>20)
	}
	if UploadAdmissionWeightBytes >= bodyBuffer+decodedSlice+decodedPixels {
		t.Errorf("wire weight %d MiB covers the pixel buffer too; the decode reservation charges it "+
			"against the same budget, so this double-counts it (#179)", UploadAdmissionWeightBytes>>20)
	}
	if CreativeDecodePeakBytes != decodedPixels {
		t.Errorf("CreativeDecodePeakBytes = %d MiB, want %d MiB", CreativeDecodePeakBytes>>20, decodedPixels>>20)
	}
	// The amplification must price the worst body at or above the wire weight.
	if MaxRequestBodyBytes*UploadAdmissionAmplification < UploadAdmissionWeightBytes {
		t.Errorf("amplification %d prices a %d MiB body at %d MiB, below the %d MiB wire weight",
			UploadAdmissionAmplification, MaxRequestBodyBytes>>20,
			(MaxRequestBodyBytes*UploadAdmissionAmplification)>>20, UploadAdmissionWeightBytes>>20)
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

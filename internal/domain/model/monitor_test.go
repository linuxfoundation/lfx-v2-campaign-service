// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

// An unclassified row is "other"; a class outside the closed set is "unknown", so a client that
// starts emitting a new or misspelled class shows up as drift instead of hiding inside "other";
// every class in the set passes through.
func TestNormalizeMonitorFailureClass(t *testing.T) {
	cases := map[MonitorFailureClass]MonitorFailureClass{
		"":                        MonitorFailureOther,
		"token abc123 revoked":    MonitorFailureUnknown,
		"Auth":                    MonitorFailureUnknown,
		MonitorFailureAuth:        MonitorFailureAuth,
		MonitorFailureThrottle:    MonitorFailureThrottle,
		MonitorFailureUpstream5xx: MonitorFailureUpstream5xx,
		MonitorFailureDecode:      MonitorFailureDecode,
		MonitorFailureNoReport:    MonitorFailureNoReport,
		MonitorFailureOther:       MonitorFailureOther,
		MonitorFailureUnknown:     MonitorFailureUnknown,
	}
	for in, want := range cases {
		if got := NormalizeMonitorFailureClass(in); got != want {
			t.Errorf("NormalizeMonitorFailureClass(%q) = %q, want %q", in, got, want)
		}
	}
}

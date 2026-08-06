// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedLog(t *testing.T) {
	dir := t.TempDir()
	if err := SeedLog(dir, "2026-07-09", "initial bundle generated."); err != nil {
		t.Fatalf("SeedLog: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "log", "2026-07-09-bundle-created.md"))
	if err != nil {
		t.Fatalf("reading log fragment: %v", err)
	}
	want := "# 2026-07-09 — bundle created\n\n**Creation** — initial bundle generated.\n"
	if string(data) != want {
		t.Errorf("log fragment = %q, want %q", data, want)
	}
}

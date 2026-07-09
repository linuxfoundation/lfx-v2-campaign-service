// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfvalidate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestValidateConformantBundle(t *testing.T) {
	dir := t.TempDir()

	fm := okf.Frontmatter{Type: "Architecture Doc", Title: "Overview", Description: "Summary."}
	if err := okf.WriteConcept(filepath.Join(dir, "overview.md"), fm, "# Overview\n\nSummary.\n"); err != nil {
		t.Fatalf("WriteConcept: %v", err)
	}

	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Overview](overview.md) - Summary.\n")
	writeFile(t, filepath.Join(dir, "log.md"), "# Log\n\n## 2026-07-09\n\n**Creation** — initial bundle.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

func TestValidateMissingType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.md"), "---\ntitle: \"Bad\"\n---\n\nNo type field.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a missing-type error")
	}
}

func TestValidateMissingFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.md"), "# No frontmatter\n\nJust prose.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a missing-frontmatter error")
	}
}

func TestValidateBadIndexBullet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* Overview - missing link syntax\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a bad-bullet error")
	}
}

func TestValidateIndexWithDisallowedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Non-root index.md files must not declare frontmatter at all.
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "---\ntype: \"Anything\"\n---\n\n# Sub\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a disallowed-frontmatter error")
	}
}

func TestValidateLogNotSorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "log.md"),
		"# Log\n\n## 2026-01-01\n\n**Update** — old.\n\n## 2026-07-09\n\n**Update** — new.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want an unsorted-log error")
	}
}

func TestValidateRealBundle(t *testing.T) {
	// Use relative path from package directory to the real bundle at repo root
	bundleDir := filepath.Join("..", "..", "docs", "knowledge")
	errs := Validate(bundleDir)
	if len(errs) != 0 {
		t.Errorf("Real bundle validation failed with %d errors:", len(errs))
		for _, e := range errs {
			t.Logf("  - %v", e)
		}
	}
}

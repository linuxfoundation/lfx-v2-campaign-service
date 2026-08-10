// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfvalidate

import (
	"os"
	"path/filepath"
	"strings"
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

func TestValidateIndexWithCRLFDisallowedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A CRLF-delimited frontmatter block must not bypass the non-root check.
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "---\r\ntype: \"Anything\"\r\n---\r\n\r\n# Sub\r\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a disallowed-frontmatter error")
	}
}

func TestValidateIndexWithEmptyFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// An empty frontmatter block declares no keys, but non-root index.md
	// must not declare a frontmatter block at all.
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "---\n---\n\n# Sub\n")

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

func TestValidateLogFragment(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"# 2026-08-05 — LFXV2-2812 find brief by event slug\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

func TestValidateLogFragmentWithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Fragments are not concepts; declaring frontmatter is disallowed.
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"---\ntype: \"Note\"\n---\n\n# 2026-08-05 — LFXV2-2812 find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a frontmatter-not-allowed error")
	}
}

func TestValidateLogFragmentBadFilename(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "find-brief.md"),
		"# 2026-08-05 — find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a bad-filename error")
	}
}

func TestValidateLogFragmentImpossibleDate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-99-99-find-brief.md"),
		"# 2026-99-99 — find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want an impossible-calendar-date error")
	}
}

func TestValidateLogFragmentHeadingDateMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"# 2026-08-04 — find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a heading-date-mismatch error")
	}
}

// writeConcept writes a concept file with the given frontmatter description.
func writeConcept(t *testing.T, path, description string) {
	t.Helper()
	fm := okf.Frontmatter{Type: "Go Package", Title: "Thing", Description: description}
	if err := okf.WriteConcept(path, fm, "# Thing\n\n"+description+"\n"); err != nil {
		t.Fatalf("WriteConcept(%s): %v", path, err)
	}
}

// The drift this check exists for: the concept file was updated by the PR that
// changed the behaviour, its index bullet was not. The index is what a reader
// consults FIRST to decide whether a concept file is worth opening, so a stale
// bullet routes them away from the file that would have corrected it.
func TestValidateIndexBulletDescriptionDrift(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing, and since last week also the other thing.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does the thing.\n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want exactly one description-drift error", errs)
	}
	// The message must carry BOTH texts: the fix is to pick the true one, and
	// the reader cannot pick without seeing them side by side.
	msg := errs[0].Error()
	for _, want := range []string{"Does the thing.", "also the other thing."} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not quote %q", msg, want)
		}
	}
}

// Equality is the rule, so a description that merely CONTAINS the bullet's text
// is still drift. Without this the check would pass on exactly the common case
// it is meant to catch — a concept file that grew a clause its bullet lacks.
func TestValidateIndexBulletDescriptionPrefixIsStillDrift(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing, plus a clause.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does the thing,\n")

	if errs := Validate(dir); len(errs) != 1 {
		t.Fatalf("Validate() = %v, want a description-drift error", errs)
	}
}

// The three link shapes that have no frontmatter description to compare
// against. Flagging any of them would make the check unusable: the root index
// links to sub-indexes and to the log directory, and index.md is forbidden
// from declaring frontmatter at all by the rule above this one.
func TestValidateIndexBulletDescriptionSkipsNonConceptTargets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "# Sub\n")
	// A concept declaring "type" but no "description" is legal under rule 1,
	// so its bullet has nothing to be compared against.
	writeFile(t, filepath.Join(dir, "bare.md"), "---\ntype: \"Note\"\n---\n\n# Bare\n")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n"+
			"* [Sub](sub/index.md) - A subtree, described however the root likes.\n"+
			"* [Bare](bare.md) - A concept that declares no description.\n"+
			"* [Log](log/) - A directory, not a file.\n"+
			"* [Spec](https://example.com/spec.md) - Somewhere outside the bundle.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

// A bullet pointing at a file that does not exist is a real defect, but a
// different one. Reporting it here would double-report the same bullet once
// link checking lands, so this check stays silent and the bullet-format rule
// still applies.
func TestValidateIndexBulletDescriptionSkipsMissingTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Gone](gone.md) - Whatever this said.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors from the description check", errs)
	}
}

// An anchor addresses a heading inside the target, not a different document,
// so the bullet still describes that document and must still match it. The
// assertion is on the DRIFTING case deliberately: a passing anchored link
// would also pass if the anchor were left on the path, because "thing.md#usage"
// is not a .md filename and the check would skip the bullet entirely.
func TestValidateIndexBulletDescriptionResolvesAnchoredLink(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md#usage) - Does something else.\n")

	if errs := Validate(dir); len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the anchored bullet's drift to be caught", errs)
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

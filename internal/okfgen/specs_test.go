// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSpecConcepts(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.md")
	content := "# Health Endpoints — Spec\n\nDefines liveness and readiness endpoints. More detail follows.\n"
	if err := os.WriteFile(specPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	destDir := filepath.Join(dir, "knowledge", "specs")
	refs, err := GenerateSpecConcepts([]string{specPath}, destDir)
	if err != nil {
		t.Fatalf("GenerateSpecConcepts: %v", err)
	}
	if len(refs) != 1 || refs[0].Title != "Health Endpoints — Spec" {
		t.Fatalf("unexpected refs: %+v", refs)
	}
	if refs[0].Description != "Defines liveness and readiness endpoints." {
		t.Errorf("Description = %q", refs[0].Description)
	}

	if _, err := os.Stat(filepath.Join(destDir, "spec.md")); err != nil {
		t.Errorf("expected spec.md to exist: %v", err)
	}
}

func TestGenerateSpecConceptsFallsBackToFileName(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(specPath, []byte("No heading here.\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	destDir := filepath.Join(dir, "knowledge", "specs")
	refs, err := GenerateSpecConcepts([]string{specPath}, destDir)
	if err != nil {
		t.Fatalf("GenerateSpecConcepts: %v", err)
	}
	if refs[0].Title != "notes.md" {
		t.Errorf("Title = %q, want fallback %q", refs[0].Title, "notes.md")
	}
}

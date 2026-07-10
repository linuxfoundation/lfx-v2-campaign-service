// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDocsConcepts(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()

	content := "# Example Doc\n\nThis is the summary sentence. More detail follows.\n"
	if err := os.WriteFile(filepath.Join(srcDir, "example.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	refs, err := GenerateDocsConcepts(srcDir, destDir)
	if err != nil {
		t.Fatalf("GenerateDocsConcepts: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if refs[0].Title != "Example Doc" {
		t.Errorf("Title = %q, want %q", refs[0].Title, "Example Doc")
	}
	if refs[0].Description != "This is the summary sentence." {
		t.Errorf("Description = %q, want %q", refs[0].Description, "This is the summary sentence.")
	}
	if refs[0].FileName != "example.md" {
		t.Errorf("FileName = %q, want %q", refs[0].FileName, "example.md")
	}

	out, err := os.ReadFile(filepath.Join(destDir, "example.md"))
	if err != nil {
		t.Fatalf("reading generated concept: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `type: "Architecture Doc"`) {
		t.Errorf("generated concept missing type frontmatter:\n%s", got)
	}
	if !strings.Contains(got, "This is the summary sentence.") {
		t.Errorf("generated concept missing description:\n%s", got)
	}
}

func TestGenerateDocsConceptsRenamesArchitecture(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()
	content := "# Arch\n\nSummary.\n"
	if err := os.WriteFile(filepath.Join(srcDir, "architecture.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := GenerateDocsConcepts(srcDir, destDir); err != nil {
		t.Fatalf("GenerateDocsConcepts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "overview.md")); err != nil {
		t.Errorf("expected overview.md to exist: %v", err)
	}
}

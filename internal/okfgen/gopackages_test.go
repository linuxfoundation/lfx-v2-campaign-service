// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractPackageDoc(t *testing.T) {
	dir := t.TempDir()
	content := "// Copyright Example\n// SPDX-License-Identifier: MIT\n\n// Package widget provides widgets.\npackage widget\n"
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	pkgName, doc, err := extractPackageDoc(dir)
	if err != nil {
		t.Fatalf("extractPackageDoc: %v", err)
	}
	if pkgName != "widget" {
		t.Errorf("pkgName = %q, want %q", pkgName, "widget")
	}
	if doc != "Package widget provides widgets." {
		t.Errorf("doc = %q, want %q", doc, "Package widget provides widgets.")
	}
}

func TestExtractPackageDocSkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "widget_test.go"), []byte("package widget\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	content := "// Package widget provides widgets.\npackage widget\n"
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	_, doc, err := extractPackageDoc(dir)
	if err != nil {
		t.Fatalf("extractPackageDoc: %v", err)
	}
	if doc != "Package widget provides widgets." {
		t.Errorf("doc = %q, want the doc from widget.go, not widget_test.go", doc)
	}
}

func TestGenerateGoPackageConcepts(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "internal", "widget")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "// Package widget provides widgets.\npackage widget\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "widget.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	destDir := filepath.Join(root, "knowledge", "code")
	refs, err := GenerateGoPackageConcepts([]string{pkgDir}, destDir)
	if err != nil {
		t.Fatalf("GenerateGoPackageConcepts: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if refs[0].Description != "Package widget provides widgets." {
		t.Errorf("Description = %q", refs[0].Description)
	}

	out, err := os.ReadFile(filepath.Join(destDir, refs[0].FileName))
	if err != nil {
		t.Fatalf("reading generated concept: %v", err)
	}
	if !strings.Contains(string(out), `type: "Go Package"`) {
		t.Errorf("missing type frontmatter:\n%s", out)
	}
}

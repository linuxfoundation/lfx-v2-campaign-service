// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontmatterRender(t *testing.T) {
	fm := Frontmatter{
		Type:        "Architecture Doc",
		Title:       "Overview",
		Description: "Summary of the service.",
		Resource:    "docs/architecture.md",
	}
	want := "---\n" +
		"type: \"Architecture Doc\"\n" +
		"title: \"Overview\"\n" +
		"description: \"Summary of the service.\"\n" +
		"resource: \"docs/architecture.md\"\n" +
		"---\n"
	if got := fm.Render(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestFrontmatterRenderOmitsEmptyFields(t *testing.T) {
	fm := Frontmatter{Type: "Go Package"}
	want := "---\ntype: \"Go Package\"\n---\n"
	if got := fm.Render(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestParseFrontmatter(t *testing.T) {
	data := []byte("---\ntype: \"Architecture Doc\"\ntitle: \"Overview\"\n---\n\nBody text.\n")
	fm, body, err := ParseFrontmatter(data)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm["type"] != "Architecture Doc" {
		t.Errorf("fm[type] = %v, want %q", fm["type"], "Architecture Doc")
	}
	if strings.TrimSpace(body) != "Body text." {
		t.Errorf("body = %q, want %q", body, "Body text.")
	}
}

func TestParseFrontmatterMissingDelimiter(t *testing.T) {
	_, _, err := ParseFrontmatter([]byte("no frontmatter here\n"))
	if err == nil {
		t.Fatal("ParseFrontmatter() = nil error, want an error")
	}
}

func TestParseFrontmatterEmptyBlock(t *testing.T) {
	fm, body, err := ParseFrontmatter([]byte("---\n---\n\nBody text.\n"))
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if len(fm) != 0 {
		t.Errorf("fm = %v, want empty map", fm)
	}
	if strings.TrimSpace(body) != "Body text." {
		t.Errorf("body = %q, want %q", body, "Body text.")
	}
}

func TestParseFrontmatterEmptyBlockNoTrailingContent(t *testing.T) {
	fm, body, err := ParseFrontmatter([]byte("---\n---"))
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if len(fm) != 0 {
		t.Errorf("fm = %v, want empty map", fm)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestParseFrontmatterCRLF(t *testing.T) {
	data := []byte("---\r\ntype: \"Architecture Doc\"\r\ntitle: \"Overview\"\r\n---\r\n\r\nBody text.\r\n")
	fm, body, err := ParseFrontmatter(data)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm["type"] != "Architecture Doc" {
		t.Errorf("fm[type] = %v, want %q", fm["type"], "Architecture Doc")
	}
	if strings.TrimSpace(body) != "Body text." {
		t.Errorf("body = %q, want %q", body, "Body text.")
	}
}

func TestWriteConcept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "overview.md")
	fm := Frontmatter{Type: "Architecture Doc", Title: "Overview", Description: "Summary."}

	if err := WriteConcept(path, fm, "# Overview\n\nSummary.\n"); err != nil {
		t.Fatalf("WriteConcept: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "---\ntype: \"Architecture Doc\"\n") {
		t.Errorf("WriteConcept() did not write frontmatter first:\n%s", got)
	}
	if !strings.Contains(got, "# Overview") {
		t.Errorf("WriteConcept() missing body:\n%s", got)
	}
}

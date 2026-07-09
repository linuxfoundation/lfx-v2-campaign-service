// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenderIndex(t *testing.T) {
	got := RenderIndex("Architecture", []IndexEntry{
		{Title: "Overview", Link: "overview.md", Description: "Service architecture."},
	})
	want := "# Architecture\n\n* [Overview](overview.md) - Service architecture.\n"
	if got != want {
		t.Errorf("RenderIndex() = %q, want %q", got, want)
	}
}

func TestWriteIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "index.md")
	if err := WriteIndex(path, "Sub", []IndexEntry{{Title: "A", Link: "a.md", Description: "desc"}}); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading index: %v", err)
	}
	if string(data) != "# Sub\n\n* [A](a.md) - desc\n" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestEntriesFromRefs(t *testing.T) {
	entries := EntriesFromRefs([]ConceptRef{{Title: "A", FileName: "a.md", Description: "d"}})
	if len(entries) != 1 || entries[0].Link != "a.md" || entries[0].Title != "A" || entries[0].Description != "d" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IndexEntry is one bullet in a generated index.md, per OKF §6.
type IndexEntry struct {
	Title       string
	Link        string
	Description string
}

// RenderIndex renders an OKF index.md body: a "# title" heading followed by
// one "* [Title](Link) - Description" bullet per entry.
func RenderIndex(title string, entries []IndexEntry) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("* [%s](%s) - %s\n", e.Title, e.Link, e.Description))
	}
	return b.String()
}

// WriteIndex renders and writes an index.md at path, creating parent
// directories as needed.
func WriteIndex(path, title string, entries []IndexEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(RenderIndex(title, entries)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// AppendNote appends a blank line followed by note to the file at path. Used
// for the root index's OKF-deviation note (see cmd/okfgen/main.go), which
// documents a bundle-specific structural choice that WriteIndex's generic
// title+bullets rendering has no place for.
func AppendNote(path, note string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s to append note: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + note); err != nil {
		return fmt.Errorf("appending note to %s: %w", path, err)
	}
	return nil
}

// EntriesFromRefs converts generator ConceptRefs into index entries for the
// same directory the concepts were written to (Link is the concept file's
// base name).
func EntriesFromRefs(refs []ConceptRef) []IndexEntry {
	entries := make([]IndexEntry, len(refs))
	for i, r := range refs {
		entries[i] = IndexEntry{Title: r.Title, Link: r.FileName, Description: r.Description}
	}
	return entries
}

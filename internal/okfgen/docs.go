// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package okfgen bootstraps an Open Knowledge Format (OKF) knowledge bundle
// from this repo's existing docs, Helm chart templates, Go packages, and
// speckit specs. It is driven by cmd/okfgen and is meant for one-time (or
// deliberate, subtree-by-subtree) bundle bootstrapping — not ongoing
// regeneration, which would overwrite hand-edited concept files.
package okfgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

// ConceptRef describes a generated concept file, for building the
// containing directory's index.md entries.
type ConceptRef struct {
	Title       string
	Description string
	// FileName is the concept file's base name (e.g. "overview.md"),
	// relative to the directory it was written into.
	FileName string
}

// docNameOverrides maps a docs/*.md base name to the base name it should
// use inside docs/knowledge/architecture/. Files not listed here keep
// their original base name.
var docNameOverrides = map[string]string{
	"architecture.md": "overview.md",
}

// GenerateDocsConcepts wraps every *.md file directly under srcDir
// (non-recursive) into an OKF concept file under destDir, and returns the
// generated concepts for index building.
func GenerateDocsConcepts(srcDir, destDir string) ([]ConceptRef, error) {
	matches, err := filepath.Glob(filepath.Join(srcDir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", srcDir, err)
	}

	var refs []ConceptRef
	for _, src := range matches {
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", src, err)
		}

		title, description := extractTitleAndDescription(string(data))

		base := filepath.Base(src)
		destName := base
		if override, ok := docNameOverrides[base]; ok {
			destName = override
		}
		destPath := filepath.Join(destDir, destName)

		relLink, err := filepath.Rel(destDir, src)
		if err != nil {
			return nil, fmt.Errorf("computing relative link for %s: %w", src, err)
		}

		fm := okf.Frontmatter{
			Type:        "Architecture Doc",
			Title:       title,
			Description: description,
			Resource:    filepath.ToSlash(src),
		}
		body := fmt.Sprintf("# %s\n\n%s\n\nSee [%s](%s) for full details.\n",
			title, description, filepath.ToSlash(src), filepath.ToSlash(relLink))

		if err := okf.WriteConcept(destPath, fm, body); err != nil {
			return nil, err
		}

		refs = append(refs, ConceptRef{Title: title, Description: description, FileName: destName})
	}
	return refs, nil
}

// extractTitleAndDescription pulls the first "# " heading as the title and
// the first non-empty, non-heading line after it as the description. The
// description is truncated to its first sentence.
func extractTitleAndDescription(content string) (title, description string) {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if title == "" {
			if strings.HasPrefix(trimmed, "# ") {
				title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		description = firstSentence(trimmed)
		break
	}
	return title, description
}

// firstSentence returns the portion of s up to and including its first
// ". " sentence break, or s unchanged if none is found.
func firstSentence(s string) string {
	if idx := strings.Index(s, ". "); idx != -1 {
		return s[:idx+1]
	}
	return s
}

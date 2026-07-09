// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package okf provides minimal support for the Open Knowledge Format
// (OKF) v0.1: rendering and parsing the YAML frontmatter block of a concept
// document, and writing concept files to disk.
//
// See https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf
// for the full specification.
package okf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the OKF YAML frontmatter block for a concept document.
// Type is required by the spec; the rest are recommended fields this
// repo's generators populate.
type Frontmatter struct {
	Type        string
	Title       string
	Description string
	Resource    string
}

// Render returns the "---"-delimited YAML frontmatter block for fm. Empty
// optional fields are omitted; Type is always written even if empty (the
// caller is responsible for supplying a non-empty Type per the spec).
func (fm Frontmatter) Render() string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("type: %q\n", fm.Type))
	if fm.Title != "" {
		b.WriteString(fmt.Sprintf("title: %q\n", fm.Title))
	}
	if fm.Description != "" {
		b.WriteString(fmt.Sprintf("description: %q\n", fm.Description))
	}
	if fm.Resource != "" {
		b.WriteString(fmt.Sprintf("resource: %q\n", fm.Resource))
	}
	b.WriteString("---\n")
	return b.String()
}

// ParseFrontmatter splits data into its YAML frontmatter block (as a generic
// map, since concept documents may carry arbitrary keys per the spec) and
// the remaining body. It returns an error if data does not start with a
// "---" delimited frontmatter block or the YAML fails to parse.
func ParseFrontmatter(data []byte) (map[string]any, string, error) {
	const delim = "---\n"
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, delim) {
		return nil, "", fmt.Errorf("missing frontmatter delimiter")
	}

	rest := s[len(delim):]
	end := strings.Index(rest, "\n---")
	if end == -1 {
		return nil, "", fmt.Errorf("unterminated frontmatter block")
	}

	yamlBlock := rest[:end]
	body := strings.TrimPrefix(rest[end+len("\n---"):], "\n")

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return nil, "", fmt.Errorf("parsing yaml frontmatter: %w", err)
	}
	return fm, body, nil
}

// WriteConcept writes a concept document at path, combining fm's rendered
// frontmatter with body. It creates parent directories as needed.
func WriteConcept(path string, fm Frontmatter, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	content := fm.Render() + "\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

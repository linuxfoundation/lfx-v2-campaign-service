// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package okfvalidate checks an Open Knowledge Format (OKF) v0.1 bundle for
// conformance per OKF SPEC.md §9: every non-reserved .md file has a
// parseable frontmatter block with a non-empty "type", index.md files
// carry no frontmatter (except an optional okf_version at the bundle root)
// and use the "* [Title](url) - description" bullet form, a reserved log.md
// uses "##"-level ISO 8601 date headings sorted newest first, and this
// bundle's log/ directory holds one dated fragment per entry instead
// ("YYYY-MM-DD-<slug>.md", no frontmatter, first H1 dated to match).
package okfvalidate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

// Validate walks bundleDir and checks it for OKF v0.1 conformance. It
// returns one error per violation found; a conformant bundle returns nil.
func Validate(bundleDir string) []error {
	var errs []error

	walkErr := filepath.WalkDir(bundleDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		if filepath.Clean(filepath.Dir(path)) == filepath.Clean(filepath.Join(bundleDir, "log")) {
			errs = append(errs, validateLogFragment(path)...)
			return nil
		}

		switch d.Name() {
		case "index.md":
			isRoot := filepath.Clean(filepath.Dir(path)) == filepath.Clean(bundleDir)
			errs = append(errs, validateIndex(path, isRoot)...)
		case "log.md":
			errs = append(errs, validateLog(path)...)
		default:
			if e := validateConcept(path); e != nil {
				errs = append(errs, e)
			}
		}
		return nil
	})
	if walkErr != nil {
		errs = append(errs, fmt.Errorf("walking %s: %w", bundleDir, walkErr))
	}

	return errs
}

// validateConcept checks OKF §9 rules 1 & 2: parseable frontmatter with a
// non-empty "type" field.
func validateConcept(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: reading file: %w", path, err)
	}
	fm, _, err := okf.ParseFrontmatter(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	t, _ := fm["type"].(string)
	if strings.TrimSpace(t) == "" {
		return fmt.Errorf("%s: frontmatter missing non-empty \"type\" field", path)
	}
	return nil
}

var indexBulletPattern = regexp.MustCompile(`^\* \[[^\]]+\]\([^)]+\) - .+$`)

// validateIndex checks OKF §9 rule 3 & the §6 bullet format: no
// frontmatter (except an optional okf_version at the bundle root), and any
// "* " line matches "* [Title](url) - description".
func validateIndex(path string, isRoot bool) []error {
	data, err := os.ReadFile(path)
	if err != nil {
		return []error{fmt.Errorf("%s: reading file: %w", path, err)}
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")

	if strings.HasPrefix(content, "---\n") {
		fm, body, err := okf.ParseFrontmatter(data)
		if err != nil {
			return []error{fmt.Errorf("%s: %w", path, err)}
		}
		if !isRoot {
			return []error{fmt.Errorf("%s: non-root index.md must not declare a frontmatter block", path)}
		}
		var errs []error
		for k := range fm {
			if k != "okf_version" {
				errs = append(errs, fmt.Errorf("%s: index.md must not declare frontmatter key %q", path, k))
			}
		}
		if len(errs) > 0 {
			return errs
		}
		content = body
	}

	var errs []error
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "* ") {
			continue
		}
		if !indexBulletPattern.MatchString(trimmed) {
			errs = append(errs, fmt.Errorf("%s: bullet %q does not match \"* [Title](url) - description\"", path, trimmed))
		}
	}
	return errs
}

var logFragmentNamePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-[A-Za-z0-9][A-Za-z0-9._-]*\.md$`)
var logFragmentHeadingPattern = regexp.MustCompile(`^# (\d{4}-\d{2}-\d{2})\b`)

// validateLogFragment checks a docs/knowledge/log/ entry: its filename
// matches "YYYY-MM-DD-<slug>.md", it declares no frontmatter block (a
// fragment is not a concept), and its first H1 begins with the same date as
// the filename.
func validateLogFragment(path string) []error {
	m := logFragmentNamePattern.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return []error{fmt.Errorf("%s: log fragment filename must match \"YYYY-MM-DD-<slug>.md\"", path)}
	}
	if _, err := time.Parse("2006-01-02", m[1]); err != nil {
		return []error{fmt.Errorf("%s: filename date %q is not a valid calendar date: %w", path, m[1], err)}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return []error{fmt.Errorf("%s: reading file: %w", path, err)}
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")

	if strings.HasPrefix(content, "---\n") {
		return []error{fmt.Errorf("%s: log fragment must not declare a frontmatter block", path)}
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		hm := logFragmentHeadingPattern.FindStringSubmatch(trimmed)
		if hm == nil {
			return []error{fmt.Errorf("%s: first heading %q does not start with \"# YYYY-MM-DD\"", path, trimmed)}
		}
		if hm[1] != m[1] {
			return []error{fmt.Errorf("%s: heading date %q does not match filename date %q", path, hm[1], m[1])}
		}
		return nil
	}

	return []error{fmt.Errorf("%s: no \"# YYYY-MM-DD\" heading found", path)}
}

var logDatePattern = regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})$`)

// validateLog checks OKF §9 rule 3 (log.md's own structure, per §7):
// "##"-level ISO 8601 date headings, sorted newest first.
func validateLog(path string) []error {
	data, err := os.ReadFile(path)
	if err != nil {
		return []error{fmt.Errorf("%s: reading file: %w", path, err)}
	}

	var dates []string
	for _, line := range strings.Split(string(data), "\n") {
		if m := logDatePattern.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			dates = append(dates, m[1])
		}
	}
	if len(dates) == 0 {
		return []error{fmt.Errorf("%s: no \"## YYYY-MM-DD\" date headings found", path)}
	}

	sorted := append([]string(nil), dates...)
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
	for i := range dates {
		if dates[i] != sorted[i] {
			return []error{fmt.Errorf("%s: date headings are not sorted newest-first (found %v)", path, dates)}
		}
	}
	return nil
}

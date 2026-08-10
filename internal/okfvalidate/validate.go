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
			errs = append(errs, validateIndex(bundleDir, path, isRoot)...)
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

// The destination excludes whitespace and angle brackets deliberately. CommonMark
// also accepts `(<thing.md>)` and `(thing.md "Title")`, and a looser class would
// capture the brackets or the title as part of the path — which then fails the
// ".md" suffix test in checkBulletDescription and skips the bullet silently. A
// link that looks valid would quietly opt out of the description-sync invariant.
// Rejecting the two exotic forms outright is cheaper than parsing them: okfgen
// emits bare paths, and the OKF §6 format documents only that form.
var indexBulletPattern = regexp.MustCompile(`^\* \[([^\]]+)\]\(([^)\s<>]+)\) - (.+)$`)

// validateIndex checks OKF §9 rule 3 & the §6 bullet format: no
// frontmatter (except an optional okf_version at the bundle root), any
// "* " line matches "* [Title](url) - description", and each bullet's
// description is verbatim the linked concept's frontmatter description.
func validateIndex(bundleDir, path string, isRoot bool) []error {
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
		m := indexBulletPattern.FindStringSubmatch(trimmed)
		if m == nil {
			errs = append(errs, fmt.Errorf("%s: bullet %q does not match \"* [Title](url) - description\" (the url must be a bare path: no spaces, angle brackets, or link title)", path, trimmed))
			continue
		}
		if e := checkBulletDescription(bundleDir, path, m[2], m[3]); e != nil {
			errs = append(errs, e)
		}
	}
	return errs
}

// checkBulletDescription requires an index bullet's description to be
// verbatim the linked concept's frontmatter "description".
//
// The two are written at different times — a concept file is edited by the PR
// that changes the behaviour it documents, its index bullet by whoever
// remembers step 2 of the CLAUDE.md checklist — so they drift silently, and
// the index is the surface an agent reads FIRST to decide which concept file
// is worth opening. A stale bullet is therefore worse than a stale concept:
// it does not merely say something out of date, it routes the reader away
// from the file that would have corrected it. Twelve of this bundle's 47 bullets
// had drifted by the time this check was written, in both directions —
// sometimes the bullet was the current text and the frontmatter the stale one.
//
// Equality is required rather than some looser containment, because any
// tolerance is a place drift can hide, and the cost of exactness is one
// mechanical edit in the same PR that changed the description.
//
// A bullet is only checked when it resolves to a readable .md file inside the
// bundle that declares a frontmatter description. That deliberately excludes
// links to directories, to a sub-index (index.md carries no frontmatter at
// all, by rule 3 above), and to anything outside the bundle. It does NOT
// excuse a concept file that simply omits its description: this bundle's 47
// concept files all declare one, and adding the "description" key to the
// required set belongs with "type" in validateConcept rather than here, where
// it would only be enforced for files that happen to be linked.
func checkBulletDescription(bundleDir, indexPath, link, bulletDesc string) error {
	// Anchors and query strings are not part of the path; a bare fragment
	// ("#section") points inside this same index, which has no frontmatter.
	target := link
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" || !strings.HasSuffix(target, ".md") || strings.Contains(target, "://") {
		return nil
	}

	// "Inside the bundle" is a scope this check has to enforce, not assume:
	// nothing upstream constrains a relative link, so "../../../etc/notes.md"
	// resolves to a real file and its frontmatter description would be quoted
	// back in a validation error — a file outside the bundle read, and partly
	// disclosed, by a checker documented to stay within it.
	resolved := filepath.Join(filepath.Dir(indexPath), filepath.FromSlash(target))
	if !withinBundle(bundleDir, resolved) {
		return nil
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		// A broken link is a real defect, but it is not this check's, and
		// reporting it here would fire a second time for the same bullet
		// once link checking lands. Silence keeps one defect to one error.
		return nil
	}
	fm, _, err := okf.ParseFrontmatter(data)
	if err != nil {
		// Unparseable frontmatter is already reported against the concept
		// file itself by validateConcept.
		return nil
	}
	conceptDesc, ok := fm["description"].(string)
	if !ok || strings.TrimSpace(conceptDesc) == "" {
		return nil
	}
	// Compared raw, not trimmed. Trimming here would be the one tolerance this
	// check claims not to have: a frontmatter description of " Summary. " would
	// satisfy a "Summary." bullet, and the diagnostic — printing both sides
	// trimmed — would then show two identical strings as a mismatch, or hide a
	// real one as a match. The padding is itself the drift to report, and %q
	// makes it visible. A bullet cannot carry trailing space (the line is
	// trimmed before matching), so the fix is always to unpad the frontmatter.
	if conceptDesc != bulletDesc {
		return fmt.Errorf("%s: bullet for %q describes it as %q, but the file's frontmatter description is %q — the two must match verbatim", indexPath, target, bulletDesc, conceptDesc)
	}
	return nil
}

// withinBundle reports whether target resolves to a path at or inside root.
//
// Symlinks are resolved on BOTH sides before comparing, because a lexical
// check alone is defeated by a symlink inside the bundle pointing out of it —
// and because the roots this runs against are themselves often symlinked
// (macOS /var/folders temp dirs), so resolving only one side would report
// every target as an escape.
func withinBundle(root, target string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	// A target that does not exist fails here rather than at ReadFile, and is
	// skipped for the same reason: a broken link is someone else's error.
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfvalidate

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestValidateConformantBundle(t *testing.T) {
	dir := t.TempDir()

	fm := okf.Frontmatter{Type: "Architecture Doc", Title: "Overview", Description: "Summary."}
	if err := okf.WriteConcept(filepath.Join(dir, "overview.md"), fm, "# Overview\n\nSummary.\n"); err != nil {
		t.Fatalf("WriteConcept: %v", err)
	}

	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Overview](overview.md) - Summary.\n")
	writeFile(t, filepath.Join(dir, "log.md"), "# Log\n\n## 2026-07-09\n\n**Creation** — initial bundle.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

func TestValidateMissingType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.md"), "---\ntitle: \"Bad\"\n---\n\nNo type field.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a missing-type error")
	}
}

func TestValidateMissingFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.md"), "# No frontmatter\n\nJust prose.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a missing-frontmatter error")
	}
}

func TestValidateBadIndexBullet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* Overview - missing link syntax\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a bad-bullet error")
	}
}

func TestValidateIndexWithDisallowedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Non-root index.md files must not declare frontmatter at all.
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "---\ntype: \"Anything\"\n---\n\n# Sub\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a disallowed-frontmatter error")
	}
}

func TestValidateIndexWithCRLFDisallowedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A CRLF-delimited frontmatter block must not bypass the non-root check.
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "---\r\ntype: \"Anything\"\r\n---\r\n\r\n# Sub\r\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a disallowed-frontmatter error")
	}
}

func TestValidateIndexWithEmptyFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// An empty frontmatter block declares no keys, but non-root index.md
	// must not declare a frontmatter block at all.
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "---\n---\n\n# Sub\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a disallowed-frontmatter error")
	}
}

func TestValidateLogNotSorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "log.md"),
		"# Log\n\n## 2026-01-01\n\n**Update** — old.\n\n## 2026-07-09\n\n**Update** — new.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want an unsorted-log error")
	}
}

func TestValidateLogFragment(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"# 2026-08-05 — LFXV2-2812 find brief by event slug\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

func TestValidateLogFragmentWithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Fragments are not concepts; declaring frontmatter is disallowed.
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"---\ntype: \"Note\"\n---\n\n# 2026-08-05 — LFXV2-2812 find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a frontmatter-not-allowed error")
	}
}

func TestValidateLogFragmentBadFilename(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "find-brief.md"),
		"# 2026-08-05 — find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a bad-filename error")
	}
}

func TestValidateLogFragmentImpossibleDate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-99-99-find-brief.md"),
		"# 2026-99-99 — find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want an impossible-calendar-date error")
	}
}

func TestValidateLogFragmentHeadingDateMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"# 2026-08-04 — find brief\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a heading-date-mismatch error")
	}
}

// The kind-marker checks below (#176) all assert on the error TEXT, not just
// on len(errs) != 0. Every other field a fragment has — its filename, its
// date, its heading — is checked by the same function, so a fixture written
// to exercise the marker can fail for one of those instead and the test still
// goes green with the marker check deleted. Naming the offending value in the
// assertion is what makes these binding: with the check reverted, the
// expected substring is absent and the test fails.

// writeFragment writes a log fragment whose filename and heading are known
// good, so only the body is in question.
func writeFragment(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "log", "2026-08-05-LFXV2-2812-find-brief.md"),
		"# 2026-08-05 — LFXV2-2812 find brief by event slug\n\n"+body)
}

// errsContain reports whether any error mentions want.
func errsContain(errs []error, want string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), want) {
			return true
		}
	}
	return false
}

func TestValidateLogFragmentRejectsAnUnknownKindMarker(t *testing.T) {
	dir := t.TempDir()
	// "Feature" is bold and well-formed, and reads like a kind. The set is
	// closed precisely so a plausible one-off does not quietly become a
	// seventh category nothing classifies.
	writeFragment(t, dir, "**Feature** — did the thing.\n")

	errs := Validate(dir)
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want an error naming the offending marker", errs)
	}
	// The report has to say what IS allowed, or the author is left guessing
	// at a set that lives in CLAUDE.md rather than in the message.
	if !errsContain(errs, "Update, Fix, Creation, Note, Verification, Docs") {
		t.Errorf("Validate() = %v, want the error to list the allowed markers", errs)
	}
}

func TestValidateLogFragmentRejectsAMissingKindMarker(t *testing.T) {
	dir := t.TempDir()
	writeFragment(t, dir, "Four review findings, all verified real before being acted on.\n")

	if errs := Validate(dir); !errsContain(errs, "is not a bold kind marker") {
		t.Errorf("Validate() = %v, want an error about the missing marker", errs)
	}
}

// claudeMDLogKinds returns the bold kind markers CLAUDE.md's knowledge-log
// step documents, in the order it lists them. It reads the step that names
// docs/knowledge/log/ and collects every backticked bold word in it, so the
// test below compares the validator against what an author is actually told.
// It captures the WHOLE bold label, not just a leading run of letters, so a
// documented "**Bug-Fix**" is compared as "Bug-Fix" and fails the match
// rather than being silently skipped.
func claudeMDLogKinds(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	start := strings.Index(text, "`docs/knowledge/log/")
	if start < 0 {
		t.Fatal("CLAUDE.md no longer names docs/knowledge/log/; update this test to find the step that documents the kind markers")
	}
	// The step runs from the list item that names the log directory to the
	// next numbered item.
	if i := strings.LastIndex(text[:start], "\n"); i >= 0 {
		start = i + 1
	}
	step := text[start:]
	if end := regexp.MustCompile(`\n\d+\. `).FindStringIndex(step); end != nil {
		step = step[:end[0]]
	}

	kinds := backtickedBoldKinds(step)
	if len(kinds) == 0 {
		t.Fatalf("found no backticked bold kind markers in CLAUDE.md's log step:\n%s", step)
	}
	return kinds
}

// backtickedBoldKinds returns, in order and without repeats, the bold label
// opening each inline-code span in text: `**Feature**` alone, or a documented
// example such as `**Feature** — did a thing`.
func backtickedBoldKinds(text string) []string {
	var kinds []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile("`\\*\\*([^*`]+)\\*\\*[^`]*`").FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			kinds = append(kinds, m[1])
		}
	}
	return kinds
}

func TestBacktickedBoldKindsReadsAMarkerWithExampleText(t *testing.T) {
	// CLAUDE.md may document a kind as a whole example entry. Missing that
	// form would let a kind documented only that way drift unnoticed.
	got := backtickedBoldKinds("Use `**Fix**`, or write `**Feature** — did a thing.`, and `**Fix** — again`.")
	if want := []string{"Fix", "Feature"}; !reflect.DeepEqual(got, want) {
		t.Errorf("backtickedBoldKinds() = %v, want %v", got, want)
	}
}

func TestValidateLogFragmentReportsAnUnclosedArchiveHTMLBlock(t *testing.T) {
	// An HTML comment or <pre> block runs to its closer however many blank
	// lines pass, so one that never closes swallows every later section.
	for name, opener := range map[string]string{
		"comment": "<!-- a note\n",
		"pre":     "<pre>\nsample\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir,
				"## 2026-08-05 (first)\n\n**Docs** — did the thing.\n\n"+opener+"\n"+
					"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")
			if errs := Validate(dir); !errsContain(errs, "HTML block") || !errsContain(errs, "is never closed") {
				t.Errorf("Validate() = %v, want the unclosed HTML block reported", errs)
			}
		})
	}
}

func TestValidateLogFragmentClosedHTMLBlocksAreNotReported(t *testing.T) {
	// A block closed on its own first line records no closure line in
	// goldmark, so it must be recognised from the source, not reported.
	for name, block := range map[string]string{
		"one-line comment": "<!-- a note -->\n",
		"one-line pre":     "<pre>sample</pre>\n",
		"multi-line pre":   "<pre>\nsample\n</pre>\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir,
				"## 2026-08-05 (first)\n\n**Docs** — did the thing.\n\n"+block+"\n"+
					"## 2026-08-04 (second)\n\n**Docs** — did the other thing.\n")
			if errs := Validate(dir); errsContain(errs, "never closed") {
				t.Errorf("Validate() = %v, want a closed HTML block accepted", errs)
			}
		})
	}
}

func TestValidateLogFragmentKindsMatchClaudeMD(t *testing.T) {
	// This is the test that pins the validator's set to CLAUDE.md's, in BOTH
	// directions. A kind documented in CLAUDE.md but missing from the
	// validator is rejected at validate time even though the author followed
	// the guide; a kind the validator accepts but CLAUDE.md never mentions is
	// a category no author is told exists. Either is the drift #176 is about.
	documented := claudeMDLogKinds(t)
	sorted := func(kinds []string) string {
		c := append([]string(nil), kinds...)
		sort.Strings(c)
		return strings.Join(c, ", ")
	}
	if got, want := sorted(logFragmentKinds), sorted(documented); got != want {
		t.Errorf("logFragmentKinds = [%s], CLAUDE.md documents [%s]; change both lists together", got, want)
	}

	for _, kind := range documented {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir, "**"+kind+"** — did the thing.\n")
			if errs := Validate(dir); len(errs) != 0 {
				t.Errorf("Validate() = %v, want documented kind %q accepted", errs, kind)
			}
		})
	}
}

func TestValidateLogFragmentReadsPastASupersededBanner(t *testing.T) {
	dir := t.TempDir()
	// A superseded entry carries its warning ABOVE the marker, so a reader
	// meets it before the entry it qualifies. The banner is bold too, so a
	// check that simply took the first non-blank line would both miss the
	// real marker below and reject the file for the banner's own word.
	writeFragment(t, dir,
		"> **Superseded — not what shipped in PR #260.** This entry records the\n"+
			"> original plan.\n\n**Update** — did the thing.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want the marker below the banner accepted", errs)
	}
}

func TestValidateLogFragmentStillChecksTheMarkerBelowABanner(t *testing.T) {
	dir := t.TempDir()
	// The banner must not become a way to skip the check entirely: what
	// follows it is still an entry and still needs a marker.
	writeFragment(t, dir,
		"> **Superseded.** See the later entry.\n\n**Feature** — did the thing.\n")

	if errs := Validate(dir); !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the marker below the banner still checked", errs)
	}
}

func TestValidateLogFragmentChecksEveryArchiveSection(t *testing.T) {
	dir := t.TempDir()
	// An archive rolls many entries under one H1, each behind its own "##"
	// heading with its own marker. The check has to descend into the
	// sections: applied at the top of the file it would find a "##" heading
	// where it wanted a marker and reject every archive, and skipping
	// archives outright would leave the bundle's largest files unchecked.
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Fix** — did the thing.\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the bad archive section reported", errs)
	}
	// Naming the section is what makes a 34-entry archive actionable: the
	// path alone does not say which entry to go and fix.
	if !errsContain(errs, "## 2026-08-04 (second)") {
		t.Errorf("Validate() = %v, want the error to name the offending section", errs)
	}
	if errsContain(errs, "## 2026-08-05 (first)") {
		t.Errorf("Validate() = %v, want the conformant section left alone", errs)
	}
}

func TestValidateLogFragmentRejectsAnEntryWithNoBody(t *testing.T) {
	// An H1 and nothing else carries no marker at all. A banner alone is the
	// same case: entryBlockAfter skips the blockquote, and there is no
	// entry under it to classify.
	for name, body := range map[string]string{
		"empty":       "",
		"banner only": "> **Superseded.** See the later entry.\n",
		"blank lines": "\n\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir, body)
			if errs := Validate(dir); !errsContain(errs, "has no body") {
				t.Errorf("Validate() = %v, want an error saying the entry has no body", errs)
			}
		})
	}
}

func TestValidateLogFragmentReportsAnArchiveSectionWithNoBody(t *testing.T) {
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Fix** — did the thing.\n\n"+
			"## 2026-08-04 (second)\n")

	errs := Validate(dir)
	if !errsContain(errs, `entry in section "## 2026-08-04 (second)" has no body`) {
		t.Errorf("Validate() = %v, want the empty archive section reported by name", errs)
	}
}

func TestValidateLogFragmentIgnoresHeadingsInsideArchiveCodeFences(t *testing.T) {
	// An archive entry that quotes markdown carries "## " lines that are
	// sample text, not sections. Matching them would demand a kind marker
	// under each and reject a conformant archive.
	for name, fence := range map[string]string{"backticks": "```", "tildes": "~~~"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir,
				"## 2026-08-05 (first)\n\n**Docs** — a sample log looks like this:\n\n"+
					fence+"markdown\n## 2026-01-01\n\nNot a marker.\n"+fence+"\n\n"+
					"## 2026-08-04 (second)\n\n**Fix** — did the other thing.\n")
			if errs := Validate(dir); len(errs) != 0 {
				t.Errorf("Validate() = %v, want a fenced \"## \" line ignored", errs)
			}
		})
	}
}

func TestValidateLogFragmentStillChecksArchiveSectionsAfterAFence(t *testing.T) {
	// The fence must close: a section after it is still a section and still
	// needs a marker. A tilde line inside a backtick block does not close it.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — sample:\n\n"+
			"```\n~~~\n## 2026-01-01\n```\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the section after the fence still checked", errs)
	}
	if errsContain(errs, "## 2026-01-01") {
		t.Errorf("Validate() = %v, want the fenced heading ignored", errs)
	}
}

func TestValidateLogFragmentFenceLookalikesDoNotHideLaterSections(t *testing.T) {
	// Each sample is something an archive entry can contain that is NOT a
	// fence opener. Treated as one, it would never close and every later
	// section would go unchecked, so each case puts a bad section after it.
	for name, sample := range map[string]string{
		"indented code block": "    ```\n",
		"inline code span":    "```go test``` runs it.\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir,
				"## 2026-08-05 (first)\n\n**Docs** — sample:\n\n"+sample+"\n"+
					"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")
			errs := Validate(dir)
			if !errsContain(errs, `kind marker "Feature"`) {
				t.Errorf("Validate() = %v, want the section after the lookalike still checked", errs)
			}
			if errsContain(errs, "never closed") {
				t.Errorf("Validate() = %v, want the lookalike not taken for a fence", errs)
			}
		})
	}
}

func TestValidateLogFragmentRecognisesAFenceInsideAListItem(t *testing.T) {
	// A sample inside a list item is fenced after the item marker, and its
	// heading is indented with the item's content. Neither the fence nor
	// the heading may be read as structure, and the section after the
	// list is still checked.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — samples:\n\n"+
			"- ```markdown\n  ## 2026-01-01\n  ```\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if errsContain(errs, "## 2026-01-01") {
		t.Errorf("Validate() = %v, want the list-contained sample heading ignored", errs)
	}
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the section after the list still checked", errs)
	}
}

func TestValidateLogFragmentReportsAnUnclosedArchiveFence(t *testing.T) {
	// An unclosed fence swallows the rest of the file. Trusting it would
	// make the sections after it a silent pass, so it is an error.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — sample:\n\n```markdown\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	if errs := Validate(dir); !errsContain(errs, "is never closed") {
		t.Errorf("Validate() = %v, want the unclosed fence reported", errs)
	}
}

// writeConcept writes a concept file with the given frontmatter description.
func writeConcept(t *testing.T, path, description string) {
	t.Helper()
	fm := okf.Frontmatter{Type: "Go Package", Title: "Thing", Description: description}
	if err := okf.WriteConcept(path, fm, "# Thing\n\n"+description+"\n"); err != nil {
		t.Fatalf("WriteConcept(%s): %v", path, err)
	}
}

// The drift this check exists for: the concept file was updated by the PR that
// changed the behaviour, its index bullet was not. The index is what a reader
// consults FIRST to decide whether a concept file is worth opening, so a stale
// bullet routes them away from the file that would have corrected it.
func TestValidateIndexBulletDescriptionDrift(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing, and since last week also the other thing.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does the thing.\n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want exactly one description-drift error", errs)
	}
	// The message must carry BOTH texts: the fix is to pick the true one, and
	// the reader cannot pick without seeing them side by side.
	msg := errs[0].Error()
	for _, want := range []string{"Does the thing.", "also the other thing."} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not quote %q", msg, want)
		}
	}
}

// Equality is the rule, so a description that merely CONTAINS the bullet's text
// is still drift. Without this the check would pass on exactly the common case
// it is meant to catch — a concept file that grew a clause its bullet lacks.
func TestValidateIndexBulletDescriptionPrefixIsStillDrift(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing, plus a clause.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does the thing,\n")

	if errs := Validate(dir); len(errs) != 1 {
		t.Fatalf("Validate() = %v, want a description-drift error", errs)
	}
}

// The three link shapes that have no frontmatter description to compare
// against. Flagging any of them would make the check unusable: the root index
// links to sub-indexes and to the log directory, and index.md is forbidden
// from declaring frontmatter at all by the rule above this one.
func TestValidateIndexBulletDescriptionSkipsNonConceptTargets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "sub", "index.md"), "# Sub\n")
	// A concept declaring "type" but no "description" is legal under rule 1,
	// so its bullet has nothing to be compared against.
	writeFile(t, filepath.Join(dir, "bare.md"), "---\ntype: \"Note\"\n---\n\n# Bare\n")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n"+
			"* [Sub](sub/index.md) - A subtree, described however the root likes.\n"+
			"* [Bare](bare.md) - A concept that declares no description.\n"+
			"* [Log](log/) - A directory, not a file.\n"+
			"* [Spec](https://example.com/spec.md) - Somewhere outside the bundle.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

// A bullet pointing at a file that does not exist is a real defect, but a
// different one. Reporting it here would double-report the same bullet once
// link checking lands, so this check stays silent and the bullet-format rule
// still applies.
func TestValidateIndexBulletDescriptionSkipsMissingTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Gone](gone.md) - Whatever this said.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors from the description check", errs)
	}
}

// An anchor addresses a heading inside the target, not a different document,
// so the bullet still describes that document and must still match it. The
// assertion is on the DRIFTING case deliberately: a passing anchored link
// would also pass if the anchor were left on the path, because "thing.md#usage"
// is not a .md filename and the check would skip the bullet entirely.
func TestValidateIndexBulletDescriptionResolvesAnchoredLink(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md#usage) - Does something else.\n")

	if errs := Validate(dir); len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the anchored bullet's drift to be caught", errs)
	}
}

// A query string is stripped for the same reason as an anchor, and is worth its
// own case because the two are stripped by one `IndexAny(target, "#?")` — a
// narrowing of that set to "#" alone leaves this bullet skipped and only this
// test notices. Asserted on the drifting case, as above.
func TestValidateIndexBulletDescriptionResolvesQueryStringLink(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md?v=2) - Does something else.\n")

	if errs := Validate(dir); len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the query-string bullet's drift to be caught", errs)
	}
}

// Nothing constrains a relative link, so a bullet can name a path that leaves
// the bundle entirely — and reading it would put a file the checker was never
// pointed at, and part of its contents, into validation output. The bullet is
// skipped rather than reported: a link that escapes the bundle is a broken
// link, and broken links are the link checker's error, not this one's (see the
// ReadFile branch). Asserted on a file whose description DIFFERS from the
// bullet, so that reading it at all is what the test would catch.
func TestValidateIndexBulletDescriptionSkipsTargetOutsideBundle(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeConcept(t, filepath.Join(root, "outside.md"), "Does the thing.")
	writeFile(t, filepath.Join(bundle, "index.md"), "# Bundle\n\n* [Outside](../outside.md) - Does something else.\n")

	if errs := Validate(bundle); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want a target outside the bundle to be left alone", errs)
	}
}

// A symlink inside the bundle is the same escape by another route, and it is
// the one a purely lexical "does the path contain ..?" check misses.
func TestValidateIndexBulletDescriptionSkipsSymlinkOutOfBundle(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeConcept(t, filepath.Join(root, "outside.md"), "Does the thing.")
	if err := os.Symlink(filepath.Join(root, "outside.md"), filepath.Join(bundle, "thing.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	writeFile(t, filepath.Join(bundle, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does something else.\n")

	if errs := Validate(bundle); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want a symlinked escape to be left alone", errs)
	}
}

// The containment check has to hold for a RELATIVE bundle root, which is how the
// command is actually invoked ("go run ./cmd/okfvalidate ./docs/knowledge").
// EvalSymlinks keeps a relative path relative only until a symlink points somewhere
// absolute; filepath.Rel then refuses to compare the two and the concept — a real,
// in-bundle file — is written off as an escape and never checked. That failure is
// silent, which is what makes it worth a test: the drift this whole check exists to
// catch simply stops being reported.
func TestValidateIndexBulletDescriptionRelativeRootWithAbsoluteSymlink(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeConcept(t, filepath.Join(bundle, "real.md"), "Does the thing.")
	// An ABSOLUTE link target, pointing back inside the bundle — legitimate, and the
	// thing that makes EvalSymlinks hand back an absolute path for a relative input.
	if err := os.Symlink(filepath.Join(bundle, "real.md"), filepath.Join(bundle, "thing.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	writeFile(t, filepath.Join(bundle, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Stale description.\n")

	t.Chdir(root)

	errs := Validate("bundle")
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the in-bundle concept to be checked like any other", errs)
	}
	if !strings.Contains(errs[0].Error(), "must match verbatim") {
		t.Errorf("Validate() error = %q, want the verbatim-mismatch diagnostic", errs[0])
	}
}

// "Verbatim" has to include whitespace or it is not verbatim. This fixture pads
// only the FRONTMATTER, which is the asymmetry the check has to catch: trimming
// it would let " Does the thing. " satisfy a bullet reading "Does the thing.",
// while the diagnostic printed both sides trimmed and so showed two identical
// strings as a mismatch. (A bullet padded to match is a separate case, covered
// by TestValidateIndexBulletTrailingSpaceIsNotTolerated.) Written by hand rather
// than through WriteConcept so the padding is unambiguously what reaches the
// parser.
func TestValidateIndexBulletDescriptionRejectsPaddedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "thing.md"), "---\ntype: Go Package\ntitle: Thing\ndescription: ' Does the thing. '\n---\n\n# Thing\n")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does the thing.\n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the padded frontmatter description to be caught", errs)
	}
	if !strings.Contains(errs[0].Error(), `" Does the thing. "`) {
		t.Errorf("Validate() error = %q, want it to quote the padding it is complaining about", errs[0])
	}
}

// CommonMark accepts `(<thing.md>)` and `(thing.md "Title")` as links to the same
// file. Both must be rejected at the bullet-format check rather than reaching the
// description comparison, where the brackets or the title travel with the path,
// the ".md" suffix test fails, and the bullet is skipped — an index entry that
// renders correctly but silently opts out of the description-sync invariant.
func TestValidateIndexBulletRejectsNonBareDestinations(t *testing.T) {
	for name, link := range map[string]string{
		"angle brackets": "<thing.md>",
		"link title":     `thing.md "Thing"`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// The frontmatter description deliberately DISAGREES with the bullet, so a
			// test that fails here fails for the right reason: were the bullet accepted
			// and its destination resolved, the drift below would be reported instead,
			// and this assertion on the format error would still be the thing that broke.
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing]("+link+") - Does something else.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want the non-bare destination to be rejected", errs)
			}
			if !strings.Contains(errs[0].Error(), "does not match") {
				t.Errorf("Validate() error = %q, want the bullet-format diagnostic", errs[0])
			}
		})
	}
}

// CommonMark interprets backslash and entity escapes in link destinations: `thing\.md`
// renders as `thing.md`, and so does `thing&#46;md`. This validator decodes neither, and
// each is silently skipped for its OWN reason — which is why they are two cases and not
// one row of a table:
//
//   - `thing\.md` survives to checkBulletDescription intact, passes the naive ".md" suffix
//     test, and then fails at filepath.Join/os.ReadFile, which find no file spelled with
//     the backslash.
//   - `thing&#46;md` never gets that far. The literal `#` reads as the start of a fragment,
//     so the target is truncated to `thing&`, which fails the ".md" suffix test and is
//     dropped before any file is looked up.
//
// Both end as a working link that quietly opted out of the description-sync invariant, so
// both must be rejected up front with a diagnostic — but by different guards, and the
// assertions below name which.
func TestValidateIndexBulletRejectsEscapedDestinations(t *testing.T) {
	for name, tc := range map[string]struct {
		link string
		// want is the fragment of the diagnostic that identifies WHICH guard fired. The
		// backslash is excluded by the destination character class; the entity is caught
		// by hasEntityRef, because the class must admit `&` for query strings.
		want string
	}{
		"backslash-escape": {link: `thing\.md`, want: "does not match"},
		"html-entity":      {link: `thing&#46;md`, want: "HTML entity"},
		// A NAMED reference fails the same way for a different reason: nothing truncates
		// it, so `thing&period;md` reaches the suffix test whole and misses ".md" — the
		// same silent skip by another route, and the reason the guard is not numeric-only.
		"named-entity": {link: `thing&period;md`, want: "HTML entity"},
		"hex-entity":   {link: `thing&#x2e;md`, want: "HTML entity"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// The frontmatter description deliberately DISAGREES with the bullet, so a
			// test that fails here fails for the right reason: were the bullet accepted
			// and its destination resolved, the drift below would be reported instead,
			// and this assertion on the format error would still be the thing that broke.
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing]("+tc.link+") - Does something else.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want the escaped destination to be rejected", errs)
			}
			if !strings.Contains(errs[0].Error(), tc.want) {
				t.Errorf("Validate() error = %q, want it to contain %q", errs[0], tc.want)
			}
		})
	}
}

// TestValidateIndexBulletAcceptsAMultiParameterQuery is the other half of the entity
// guard, and the reason that guard cannot just be an `&` banned from the whole
// destination. checkBulletDescription documents query strings as supported and strips
// them before resolving the path, so a second query parameter — which can only be
// introduced by an `&` — must reach the description comparison like any bare path.
// Excluding `&` in the destination character class rejected this bullet at the FORMAT
// stage, contradicting the behaviour the resolver goes out of its way to support.
func TestValidateIndexBulletAcceptsAMultiParameterQuery(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n* [Thing](thing.md?v=1&lang=en) - Does the thing.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want a multi-parameter query to be accepted", errs)
	}

	// And the query really is stripped rather than tolerated: the same destination with a
	// bullet that DISAGREES with the frontmatter must still be caught, or the test above
	// would pass just as well for a destination nothing ever resolved.
	drift := t.TempDir()
	writeConcept(t, filepath.Join(drift, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(drift, "index.md"),
		"# Bundle\n\n* [Thing](thing.md?v=1&lang=en) - Does something else.\n")

	errs := Validate(drift)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the drifted description to be reported", errs)
	}
	if !strings.Contains(errs[0].Error(), "description") {
		t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
	}
}

// TestValidateIndexBulletAcceptsALiteralAmpersandInThePath is the over-rejection half of
// the entity guard. The first version keyed on the `&` alone, which made "is this an
// entity?" the same question as "is there an ampersand?" — and `&` is a perfectly legal
// PATH character, so a concept file genuinely named `research&development.md`, linked
// correctly and bare, was refused with a diagnostic about an entity it does not contain.
//
// Over-rejection is the more tempting mistake here because it looks conservative, and it
// is not: the guard exists to stop a link SILENTLY opting out of description-sync, and
// refusing a link that would have passed sync serves nothing. The closing `;` is what
// separates the two — an entity has one, a bare ampersand does not.
func TestValidateIndexBulletAcceptsALiteralAmpersandInThePath(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "research&development.md"), "Covers R&D.")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n* [R and D](research&development.md) - Covers R&D.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want a literal ampersand in the path to be accepted", errs)
	}

	// And it is resolved, not merely tolerated: drift against the same destination must
	// still be reported, or the assertion above would hold for a path nothing looked up.
	drift := t.TempDir()
	writeConcept(t, filepath.Join(drift, "research&development.md"), "Covers R&D.")
	writeFile(t, filepath.Join(drift, "index.md"),
		"# Bundle\n\n* [R and D](research&development.md) - Covers something else.\n")

	errs := Validate(drift)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the drifted description to be reported", errs)
	}
	if !strings.Contains(errs[0].Error(), "description") {
		t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
	}
}

// TestValidateIndexBulletAcceptsAnEntityOutsideThePath is the second over-rejection the entity
// guard has had to price, found by Cursor Bugbot on PR #115 immediately after the first.
//
// Scoping the check to the RAW destination fixed the numeric form — `thing&#46;md` hides its
// reference behind the same `#` destinationPath truncates at — but dragged the query and the
// fragment in with it. Neither reaches path resolution: checkBulletDescription strips both
// before looking anything up, so `thing.md?a=1&amp;b=2` resolves exactly as `thing.md` does and
// refusing it refuses nothing dangerous. That is the same argument that motivated the bare-`&`
// fix one commit earlier, applied to the region the fix moved the check into.
//
// Both fixtures carry a DRIFTED description, so the assertion is that the link was resolved and
// compared — not merely that no format error was raised, which would also hold if the bullet had
// been skipped for a different reason.
func TestValidateIndexBulletAcceptsAnEntityOutsideThePath(t *testing.T) {
	for name, link := range map[string]string{
		"an entity in the query":    "thing.md?a=1&amp;b=2",
		"an entity in the fragment": "thing.md#sec&amp;more",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Thing]("+link+") - Says something else.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want exactly the description-drift error — an entity "+
					"outside the path never reaches resolution, so the bullet must be resolved "+
					"and compared like any other", errs)
			}
			if strings.Contains(errs[0].Error(), "HTML entity") {
				t.Fatalf("Validate() error = %q, want the description diagnostic: this link "+
					"resolves correctly and is being refused for an entity that cannot affect it",
					errs[0])
			}
			if !strings.Contains(errs[0].Error(), "description") {
				t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
			}
		})
	}
}

// TestValidateIndexBulletAcceptsAMalformedEscapeOutsideThePath pins the case that forced
// classification into the path region in the first place.
//
// The entity fixtures above travel through `html.UnescapeString` and come out as ordinary
// characters, so they never test what `url.Parse` does with input it considers malformed.
// A percent sign that is not the start of a valid escape does: `url.Parse("thing.md#100%")`
// returns `invalid URL escape "%"`, and it returns it for a percent ANYWHERE in the input,
// fragment included. `100%` is a legal CommonMark fragment — a heading named "100%" gets
// exactly that anchor — so classifying the whole destination reported a resolvable bullet as
// "not a URL reference".
//
// The fixture carries a DRIFTED description for the same reason as the test above: asserting
// only that no format error was raised would also pass if the bullet had been skipped.
//
// Revert-verified, and only the FRAGMENT case fails against the old classification. The query
// case passes either way — `url.Parse` tolerates a bare `%` in a query and never refused it —
// and is kept as a boundary marker for the other region resolution discards, labelled as one
// because a subtest that cannot fail reads as evidence until someone checks.
func TestValidateIndexBulletAcceptsAMalformedEscapeOutsideThePath(t *testing.T) {
	for name, link := range map[string]string{
		"a bare percent in the fragment":              "thing.md#100%",
		"a bare percent in the query (boundary only)": "thing.md?pct=100%",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Thing]("+link+") - Says something else.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want exactly the description-drift error — a malformed "+
					"escape outside the path never reaches resolution, so the bullet must be "+
					"resolved and compared like any other", errs)
			}
			if strings.Contains(errs[0].Error(), "not a URL reference") {
				t.Fatalf("Validate() error = %q, want the description diagnostic: this link "+
					"resolves correctly and is being refused for a region no later step reads",
					errs[0])
			}
			if !strings.Contains(errs[0].Error(), "description") {
				t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
			}
		})
	}
}

// TestValidateIndexBulletAcceptsAnEntityShapedLiteral is the THIRD over-rejection the entity
// guard has had to price, and the one that shows shape was never the discriminator.
//
// `&notAnHtmlEntity;` looks exactly like a named reference and is not one: CommonMark decodes a
// named reference only when the name is in the HTML5 entity table, so this text stays literal and
// `research&notAnHtmlEntity;.md` names a file that resolves. Refusing it answers "that link is
// malformed" about a link that is not — the same failure mode as the bare-`&` and
// entity-in-the-query rounds, one level further in.
//
// The two hard cases are the reverse direction, and they are why the check is not the obvious
// `html.UnescapeString(c) != c`: Go's decoder honours the HTML5 LEGACY forms that need no
// semicolon, so it rewrites `&notAnHtmlEntity;` to `¬AnHtmlEntity;` — a prefix match CommonMark
// never performs — while `&semi;` and `&#59;` decode TO a semicolon and would fail a cruder
// "the decode must not end in `;`" test. charRef asks whether the trailing `;` was CONSUMED,
// which separates all three.
//
// The accepted fixtures carry a DRIFTED description, so the assertion is that the link was
// resolved and compared, not merely that no format error was raised.
func TestValidateIndexBulletAcceptsAnEntityShapedLiteral(t *testing.T) {
	for name, base := range map[string]string{
		"an unknown name":                "research&notAnHtmlEntity;",
		"a name that is a legacy prefix": "research&nots;",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, base+".md"), "Covers R&D.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [R and D]("+base+".md) - Says something else.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want exactly the description-drift error — %q is not an "+
					"HTML5 named reference, so the path is literal and resolves", errs, base)
			}
			if strings.Contains(errs[0].Error(), "HTML entity") {
				t.Fatalf("Validate() error = %q, want the description diagnostic: this link resolves "+
					"correctly and is being refused for a reference it does not contain", errs[0])
			}
			if !strings.Contains(errs[0].Error(), "description") {
				t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
			}
		})
	}

	// The reverse direction, so the fix above cannot be satisfied by dropping the guard: a name
	// that IS in the table must still be refused, including the two whose replacement is itself a
	// semicolon and which a "decode must not end in `;`" test would wave through.
	for name, base := range map[string]string{
		"a named reference":          "thing&period;",
		"a named semicolon":          "thing&semi;",
		"a numeric semicolon":        "thing&#59;",
		"a legacy name with its own": "thing&not;",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Thing]("+base+"md) - Does the thing.\n")

			errs := Validate(dir)
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "HTML entity") {
				t.Fatalf("Validate() = %v, want the HTML-entity diagnostic: %q is a real character "+
					"reference, and this validator does not decode one — so the bullet would be "+
					"skipped without a word", errs, base)
			}
		})
	}
}

// TestValidateIndexBulletAcceptsAnEntityDenotingASeparator pins the SIXTH over-rejection, and
// the one this PR's own decoding work created the opening for.
//
// `&num;` and `&quest;` denote `#` and `?` without spelling either, so CommonMark resolves
// `thing.md&num;usage` exactly like `thing.md#usage`: the reference sits in the FRAGMENT and the
// path is a plain `thing.md` that this validator resolves and compares without decoding anything.
// pathRegion looked for separators in the raw spelling alone, found none, and reported a path
// entity in a link that works — the entity-in-the-query round again, arriving by the route that
// opened the moment classification began decoding.
//
// The fixtures carry a DRIFTED description, so the assertion is that the link was resolved and
// compared rather than merely not refused on format.
func TestValidateIndexBulletAcceptsAnEntityDenotingASeparator(t *testing.T) {
	for name, link := range map[string]string{
		"a named fragment marker":   "thing.md&num;usage",
		"a named query marker":      "thing.md&quest;v=2",
		"a numeric fragment marker": "thing.md&#35;usage",
		"a numeric query marker":    "thing.md&#63;v=2",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Thing]("+link+") - Says something else.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want exactly the description-drift error — %q denotes a "+
					"separator, so the path is a bare thing.md that resolves", errs, link)
			}
			if strings.Contains(errs[0].Error(), "HTML entity") {
				t.Fatalf("Validate() error = %q, want the description diagnostic: the reference is in "+
					"the fragment or query, not the path", errs[0])
			}
			if !strings.Contains(errs[0].Error(), "description") {
				t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
			}
		})
	}
}

// TestValidateIndexBulletAcceptsAnOverlongNumericReference pins the FIFTH over-rejection: a
// numeric reference longer than CommonMark admits.
//
// CommonMark takes 1–7 decimal digits and 1–6 hexadecimal ones. `&#00000046;` has eight, so it
// stays literal — which makes its `#` an ordinary fragment marker and the destination's path
// `thing&`, a target naming no concept file. Bullets pointing outside the bundle are skipped, and
// this one should have been. html.UnescapeString decodes the longer run anyway, so an unbounded
// pattern saw a reference where the renderer sees none and refused a bullet the validator has no
// claim over.
//
// Zero errors is the whole assertion: the description is drifted precisely so that a bullet
// wrongly treated as naming thing.md would be caught here too.
func TestValidateIndexBulletAcceptsAnOverlongNumericReference(t *testing.T) {
	for name, link := range map[string]string{
		"eight decimal digits":  "thing&#00000046;md",
		"seven hexadecimal":     "thing&#x000002e;md",
		"a bounded one is real": "thing&#46;md",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Thing]("+link+") - Says something else.\n")

			errs := Validate(dir)
			if name == "a bounded one is real" {
				// The reverse direction, so the bound cannot be satisfied by dropping the guard.
				if len(errs) != 1 || !strings.Contains(errs[0].Error(), "HTML entity") {
					t.Fatalf("Validate() = %v, want the HTML-entity diagnostic: %q is within "+
						"CommonMark's seven decimal digits and decodes to thing.md", errs, link)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("Validate() = %v, want none — CommonMark leaves %q literal, so its `#` "+
					"begins a fragment and the path is %q, which names no concept file",
					errs, link, "thing&")
			}
		})
	}
}

// TestValidateIndexBulletAcceptsAnEntityInAnExternalDestination is the entity guard's FOURTH
// over-rejection, after the bare `&`, the entity in a query and the entity-shaped literal.
//
// The guard's argument — this validator cannot decode a reference, so it cannot classify a path
// carrying one — is about a path in THIS bundle. An external destination names no concept file:
// checkBulletDescription returns before resolving anything with a scheme or an authority, so the
// bullet is never compared and an entity in it cannot bypass a comparison that never happens.
// Refusing it reported a defect in a link that works.
func TestValidateIndexBulletAcceptsAnEntityInAnExternalDestination(t *testing.T) {
	for name, link := range map[string]string{
		"an absolute URL":       "https://example.com/a&amp;b.md",
		"a protocol-relative":   "//example.com/a&amp;b.md",
		"a scheme without host": "mailto:notes&amp;more.md",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Spec]("+link+") - An external spec.\n")

			if errs := Validate(dir); len(errs) != 0 {
				t.Fatalf("Validate() = %v, want an external destination to be left alone: it names "+
					"no concept file, so it is never resolved and never compared", errs)
			}
		})
	}
}

// TestValidateIndexBulletRejectsAnUnparseableDestination closes the last silent opt-out in this
// class.
//
// CommonMark's destination grammar accepts a bare `%`, so `[Thing](thing%.md)` IS a link — but
// `url.Parse` rejects it, and checkBulletDescription read that rejection as "not a link to
// anything, so nothing to compare against" and returned nil. A bullet could therefore drift from
// its target's description forever, reported by nothing, for the sake of one character. That is
// the same failure the backslash, angle-bracket and paren rules all exist to prevent, reached
// through the parser instead of through the character class.
//
// The fixture's description is DELIBERATELY wrong, so a passing Validate would mean the bullet
// was skipped rather than that it agreed with its target.
func TestValidateIndexBulletRejectsAnUnparseableDestination(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing%.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n* [Thing](thing%.md) - COMPLETELY DIFFERENT TEXT.\n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the unparseable destination to be reported rather than "+
			"silently skipped past description sync", errs)
	}
	if !strings.Contains(errs[0].Error(), "not a URL reference") {
		t.Errorf("Validate() error = %q, want the unparseable-destination diagnostic", errs[0])
	}
}

// TestValidateIndexBulletResolvesThroughAFragmentThatCannotBeParsed prices the sixth
// over-rejection in this class, and it is the one that reached furthest: it refused a bullet over
// a region no step of this validator reads.
//
// url.Parse rejects a malformed percent-escape ANYWHERE in its input, fragment included, and
// classifyDestination was handed the whole destination. So `thing.md#100%` — a legal CommonMark
// link whose path is a bare `thing.md` — came back destUnparseable and was reported as "not a URL
// reference". checkBulletDescription strips the fragment before resolving, so the region that
// caused the refusal is one it would have discarded a line later.
//
// The distinction the fix rests on is between a `%` in the PATH and a `%` after it. The former
// really does defeat resolution and stays rejected — TestValidateIndexBulletRejectsAnUnparseable-
// Destination pins that direction, and the two tests must be read together or this one looks like
// a loosening.
//
// Every fixture carries a DELIBERATELY WRONG description, so a silent pass would mean the bullet
// was skipped rather than that it agreed with its target. The test demands the mismatch diagnostic
// specifically: reaching the comparison is the property, not merely surviving classification.
//
// Only two of the three are regression witnesses, measured by reverting: the two fragment cases
// fail with the false diagnostic, the query case passes either way because url.Parse tolerates a
// bare `%` in a query and never did refuse it. It stays as a boundary marker — the query is the
// other region resolution discards, and nothing but a test says the two are treated alike.
func TestValidateIndexBulletResolvesThroughAFragmentThatCannotBeParsed(t *testing.T) {
	for name, link := range map[string]string{
		"a bare percent in the fragment": "thing.md#100%",
		"an entity denoting one":         "thing.md#&percnt;",
		"a bare percent in the query":    "thing.md?pct=100%",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"),
				"# Bundle\n\n* [Thing]("+link+") - COMPLETELY DIFFERENT TEXT.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want exactly the description mismatch: the path region "+
					"is a bare thing.md and resolves", errs)
			}
			if strings.Contains(errs[0].Error(), "not a URL reference") {
				t.Fatalf("Validate() error = %q, want the bullet to reach description comparison "+
					"rather than be refused over a region resolution discards", errs[0])
			}
			if !strings.Contains(errs[0].Error(), "frontmatter description") {
				t.Errorf("Validate() error = %q, want the description-sync diagnostic", errs[0])
			}
		})
	}
}

// TestValidateIndexBulletRejectsAnUnbalancedOpeningParen pins the destination class against the
// opening parenthesis, whose omission let this validator see a link where CommonMark sees none.
//
// CommonMark permits `(` in an unbracketed destination only as part of a BALANCED pair, so
// `* [Thing](thing(foo.md) - Summary.` is not a link: the destination runs to a `)` that never
// arrives. With `(` accepted, this was the only reader that parsed a bullet there, and it went on
// to resolve `thing(foo.md` — a path that cannot exist, since okfgen never emits one. The bullet
// silently opted out of the description-sync invariant the format check exists to enforce, which
// is the same outcome as every other destination shape this class refuses.
func TestValidateIndexBulletRejectsAnUnbalancedOpeningParen(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n* [Thing](thing(foo.md) - Does the thing.\n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the malformed bullet to be reported: CommonMark does not "+
			"parse an unbalanced `(` as a link destination, so nothing here should be treated as "+
			"one", errs)
	}
}

// TestValidateIndexBulletRejectsANumericEntityBeforeAFragment is why the boundary is computed
// rather than taken from strings.IndexAny(link, "#?"). A numeric reference's own `#` is not a
// fragment marker, so a destination carrying both has to have the reference found in what
// precedes the REAL fragment. Scoping the check to destinationPath would miss this; scoping it
// to the raw destination would catch it for the wrong reason and catch the test above with it.
func TestValidateIndexBulletRejectsANumericEntityBeforeAFragment(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Bundle\n\n* [Thing](thing&#46;md#sec) - Does the thing.\n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the escaped destination to be reported", errs)
	}
	if !strings.Contains(errs[0].Error(), "HTML entity") {
		t.Errorf("Validate() error = %q, want the HTML-entity diagnostic", errs[0])
	}
}

// TestValidateIndexBulletTrailingSpaceIsNotTolerated pins the symmetric half of the
// verbatim invariant. Padded FRONTMATTER was already rejected; a padded BULLET was
// not, because the line was trimmed on both sides before matching.
func TestValidateIndexBulletTrailingSpaceIsNotTolerated(t *testing.T) {
	dir := t.TempDir()
	writeConcept(t, filepath.Join(dir, "thing.md"), "Does the thing.")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing](thing.md) - Does the thing.   \n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the padded bullet to be reported", errs)
	}
	if !strings.Contains(errs[0].Error(), "must match verbatim") {
		t.Errorf("Validate() error = %q, want the verbatim-mismatch diagnostic", errs[0])
	}
}

// TestValidateIndexBulletRejectsBlankDescription pins the case a whitespace-only
// description would otherwise slip through: the target has no comparable
// description of its own (same fixture shape as SkipsNonConceptTargets), so
// checkBulletDescription's silent early-return would leave a blank bullet
// unreported. The pattern's "(.+)$" only requires one character, so this has to
// be caught before checkBulletDescription is ever called.
func TestValidateIndexBulletRejectsBlankDescription(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bare.md"), "---\ntype: \"Note\"\n---\n\n# Bare\n")
	writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Bare](bare.md) -    \n")

	errs := Validate(dir)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %v, want the blank bullet description to be rejected", errs)
	}
	if !strings.Contains(errs[0].Error(), "blank description") {
		t.Errorf("Validate() error = %q, want the blank-description diagnostic", errs[0])
	}
}

// TestValidateIndexBulletDecodesPercentEscapes pins the ORDER of the two tests on a
// destination: it has to be parsed as a URL reference before its shape is judged, or a
// percent escape becomes a way to opt a working link out of description sync. Both
// fixtures below are links a markdown renderer resolves to the concept file, and both
// carry a description that does not match it.
func TestValidateIndexBulletDecodesPercentEscapes(t *testing.T) {
	for name, tc := range map[string]struct{ file, link string }{
		// "%2E" is ".", so the raw text does not end in ".md" and a suffix test
		// applied first skips the bullet entirely.
		"escaped-dot": {"thing.md", "thing%2Emd"},
		// "%20" is a space; read raw, this names a file that does not exist, and a
		// missing file is silently tolerated as somebody else's broken link.
		"escaped-space": {"my thing.md", "my%20thing.md"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConcept(t, filepath.Join(dir, tc.file), "Does the thing.")
			writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Thing]("+tc.link+") - Stale description.\n")

			errs := Validate(dir)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %v, want the escaped link to be checked like any other", errs)
			}
			if !strings.Contains(errs[0].Error(), "must match verbatim") {
				t.Errorf("Validate() error = %q, want the verbatim-mismatch diagnostic", errs[0])
			}
		})
	}
}

// TestValidateIndexBulletSkipsExternalDestinations covers the destinations that carry
// no "://" yet are not bundle-relative paths. Each fixture writes a local file at the
// name filepath.Join would produce, so a guard that lets one of these through resolves
// to that file and reports a drift — the assertion below is what catches it.
//
// The two entity variants are the site root's version of the external-destination
// over-rejection: the entity guard's premise is that an undecoded reference cannot be
// classified as a path IN THIS BUNDLE, and a site-root path is not in this bundle. The
// percent-encoded spelling is here because the site-root test is on the DECODED path —
// "%2Fspec..." denotes "/spec...", and judging the raw text instead would classify the
// two spellings of one destination differently.
//
// The last three carry that same rule one level up, to the part of the destination that
// MAKES it external. CommonMark decodes a character reference in a link destination, so
// "&sol;spec.md" IS "/spec.md" and "https&colon;//..." IS an absolute URL; classifying the
// raw text put each in destLocal, where the entity guard refused a link that renders
// perfectly. Each decoy sits where filepath.Join would land if the raw spelling were ever
// treated as a bundle path, so a fix that merely stops REJECTING these — without also
// stopping them being RESOLVED — reports a drift here instead of passing.
func TestValidateIndexBulletSkipsExternalDestinations(t *testing.T) {
	for name, tc := range map[string]struct{ link, decoy string }{
		"protocol-relative":                {"//host/spec.md", filepath.Join("host", "spec.md")},
		"site-root":                        {"/spec.md", "spec.md"},
		"scheme-no-slashes":                {"mailto:notes.md", "mailto:notes.md"},
		"site-root with an entity":         {"/spec&amp;notes.md", "spec&amp;notes.md"},
		"encoded site-root with an entity": {"%2Fspec&amp;notes.md", "spec&amp;notes.md"},
		"named-entity site-root":           {"&sol;spec.md", "&sol;spec.md"},
		"numeric-entity site-root":         {"&#47;spec.md", "spec.md"},
		"named-entity scheme":              {"https&colon;//example.com/spec.md", filepath.Join("https&colon;", "example.com", "spec.md")},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			decoy := filepath.Join(dir, tc.decoy)
			if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			writeConcept(t, decoy, "The decoy the bullet never named.")
			writeFile(t, filepath.Join(dir, "index.md"), "# Bundle\n\n* [Spec]("+tc.link+") - An external reference.\n")

			if errs := Validate(dir); len(errs) != 0 {
				t.Errorf("Validate() = %v, want an external destination to be skipped", errs)
			}
		})
	}
}

func TestValidateRealBundle(t *testing.T) {
	// Use relative path from package directory to the real bundle at repo root
	bundleDir := filepath.Join("..", "..", "docs", "knowledge")
	errs := Validate(bundleDir)
	if len(errs) != 0 {
		t.Errorf("Real bundle validation failed with %d errors:", len(errs))
		for _, e := range errs {
			t.Logf("  - %v", e)
		}
	}
}

func TestValidateLogFragmentQuotedFenceLookalikeDoesNotCloseATopLevelFence(t *testing.T) {
	// A literal "> ```" inside a top-level fence is content: only a closer at
	// the opener's blockquote depth ends the block, so a valid archive passes.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — sample:\n\n```markdown\n> ```\n## 2026-01-01\n```\n\n"+
			"## 2026-08-04 (second)\n\n**Docs** — did the other thing.\n")

	if errs := Validate(dir); len(errs) != 0 {
		t.Errorf("Validate() = %v, want a valid archive accepted", errs)
	}
}

func TestValidateLogFragmentBlockquoteFenceClosesWithoutASpaceAfterTheMarker(t *testing.T) {
	// A blockquote marker needs no following space, so ">```" closes a fence
	// opened as "> ```" and the section after the quote is still checked.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — quoted sample:\n\n> ```markdown\n> ## 2026-01-01\n>```\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if errsContain(errs, "never closed") {
		t.Errorf("Validate() = %v, want the quoted fence closed by >```", errs)
	}
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the section after the quote still checked", errs)
	}
}

func TestValidateLogFragmentIndentedHeadingDoesNotSelectArchiveMode(t *testing.T) {
	// "    ## 2026-01-01" is indented code, not a section, so it must not switch
	// the fragment into archive mode and skip the single-entry marker check.
	dir := t.TempDir()
	writeFragment(t, dir, "    ## 2026-01-01\n\n**Feature** — did a thing.\n")

	if errs := Validate(dir); !errsContain(errs, "kind marker") {
		t.Errorf("Validate() = %v, want the single-entry kind marker still checked", errs)
	}
}

func TestValidateLogFragmentListMarkerLookalikeDoesNotCloseATopLevelFence(t *testing.T) {
	// Container markers are not parsed inside a fenced block, so "- ```"
	// inside a top-level fence is content: it must not close the fence and
	// let the sample's real closer open a new one that hides later sections.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — sample:\n\n```markdown\n- ```\n## 2026-01-01\n```\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if errsContain(errs, "## 2026-01-01") || errsContain(errs, "never closed") {
		t.Errorf("Validate() = %v, want the list-marker lookalike treated as fence content", errs)
	}
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the section after the fence still checked", errs)
	}
}

func TestValidateLogFragmentListFenceClosesOnItsIndentedCloser(t *testing.T) {
	// A fence opened after an ordered-list marker sits at the item's content
	// column, so its closer is written at that column too: "10. ```text"
	// closes on "    ```". Missing that closer reports a valid archive as an
	// unclosed fence and leaves the section after the list unchecked.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — samples:\n\n"+
			"10. ```text\n    ## 2026-01-01\n    ```\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if errsContain(errs, "never closed") || errsContain(errs, "## 2026-01-01") {
		t.Errorf("Validate() = %v, want the list fence closed by its indented closer", errs)
	}
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the section after the list still checked", errs)
	}
}

func TestValidateLogFragmentFenceLookalikesInHTMLBlocksAreNotDelimiters(t *testing.T) {
	// A line of backticks inside an HTML block is HTML, not a fence. Read as
	// delimiters, the first lookalike opens a "fence" that the second closes,
	// and the real section between them, bad marker and all, silently passes.
	for name, wrap := range map[string][2]string{
		"comment": {"<!--\n", "-->\n"},
		"pre":     {"<pre>\n", "</pre>\n"},
	} {
		t.Run(name, func(t *testing.T) {
			lookalike := wrap[0] + "```\n" + wrap[1]
			dir := t.TempDir()
			writeFragment(t, dir,
				"## 2026-08-05 (first)\n\n**Docs** — did the thing.\n\n"+lookalike+"\n"+
					"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n\n"+lookalike)

			if errs := Validate(dir); !errsContain(errs, `kind marker "Feature"`) {
				t.Errorf("Validate() = %v, want the section between the HTML blocks checked", errs)
			}
		})
	}
}

func TestValidateLogFragmentContainerFenceAtEndOfFileIsNotUnclosed(t *testing.T) {
	// A fence inside a blockquote or list item ends with its container, so it
	// hides nothing even when the container is the file's last block and only
	// blank lines follow it.
	for name, tail := range map[string]string{
		"blockquote": "> ```markdown\n> ## 2026-01-01\n\n\n",
		"list item":  "- ```markdown\n  ## 2026-01-01\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFragment(t, dir,
				"## 2026-08-05 (first)\n\n**Docs** — did the thing.\n\n"+
					"## 2026-08-04 (second)\n\n**Docs** — quoted sample:\n\n"+tail)

			if errs := Validate(dir); errsContain(errs, "never closed") {
				t.Errorf("Validate() = %v, want a fence ended by its container not reported", errs)
			}
		})
	}
}

func TestValidateLogFragmentBlockquoteFenceClosesAfterATab(t *testing.T) {
	// A blockquote marker may be followed by a tab, so ">\t```" closes a fence
	// opened as "> ```" and the section after the quote is still checked.
	dir := t.TempDir()
	writeFragment(t, dir,
		"## 2026-08-05 (first)\n\n**Docs** — quoted sample:\n\n> ```markdown\n> ## 2026-01-01\n>\t```\n\n"+
			"## 2026-08-04 (second)\n\n**Feature** — did the other thing.\n")

	errs := Validate(dir)
	if errsContain(errs, "never closed") {
		t.Errorf("Validate() = %v, want the quoted fence closed by >\\t```", errs)
	}
	if !errsContain(errs, `kind marker "Feature"`) {
		t.Errorf("Validate() = %v, want the section after the quote still checked", errs)
	}
}

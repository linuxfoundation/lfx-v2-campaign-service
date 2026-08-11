// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfvalidate

import (
	"os"
	"path/filepath"
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
		// by hasEntityInPath, because the class must admit `&` for query strings.
		want string
	}{
		"backslash-escape": {link: `thing\.md`, want: "does not match"},
		"html-entity":      {link: `thing&#46;md`, want: "HTML entity"},
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
func TestValidateIndexBulletSkipsExternalDestinations(t *testing.T) {
	for name, tc := range map[string]struct{ link, decoy string }{
		"protocol-relative": {"//host/spec.md", filepath.Join("host", "spec.md")},
		"site-root":         {"/spec.md", "spec.md"},
		"scheme-no-slashes": {"mailto:notes.md", "mailto:notes.md"},
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

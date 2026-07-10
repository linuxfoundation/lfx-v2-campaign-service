# OKF Knowledge Bundle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a generator (`cmd/okfgen`) that bootstraps an OKF-conformant `docs/knowledge/` bundle from this repo's existing docs, Helm chart templates, Go packages, and speckit specs; a validator (`cmd/okfvalidate`) that checks bundle conformance; a CI workflow that runs the validator on PRs; and updated `CLAUDE.md`/`AGENTS.md`/`README.md` instructing agents and developers how to keep the bundle current.

**Architecture:** Two small, independently-testable Go packages — `internal/okf` (frontmatter render/parse + concept file writing, shared) and `internal/okfgen` (per-source-type generators + index/log writers) — driven by a thin `cmd/okfgen/main.go` orchestrator with the source lists hardcoded (this is a one-time bootstrap tool, not a generic framework). A third package, `internal/okfvalidate`, implements the OKF §9 conformance checks, driven by `cmd/okfvalidate/main.go`.

**Tech Stack:** Go 1.25 (stdlib `go/parser`/`go/ast`, `regexp`, `path/filepath`), `gopkg.in/yaml.v3` for frontmatter parsing, GitHub Actions.

**Design doc:** [`docs/superpowers/specs/2026-07-09-okf-knowledge-bundle-design.md`](../specs/2026-07-09-okf-knowledge-bundle-design.md)

## Global Constraints

- Go version: 1.25 (per `go.mod`; use `go-version-file: go.mod` in any new workflow).
- Every new `.go` file starts with the license header used throughout this repo:
  ```go
  // Copyright The Linux Foundation and each contributor to LFX.
  // SPDX-License-Identifier: MIT
  ```
- Every new Go package has a doc comment directly above its `package` clause
  (no blank line between the comment and `package X`), matching existing
  packages like `internal/service`.
- New `.md` files under `docs/knowledge/` do **not** need a license header —
  existing `docs/*.md` files in this repo have none, and the license-header
  check does not enforce headers on markdown.
- New GitHub Actions workflow files start with:
  ```yaml
  # Copyright The Linux Foundation and each contributor to LFX.
  # SPDX-License-Identifier: MIT
  ```
  and pin actions by SHA with a version comment, matching
  `.github/workflows/ko-build-branch.yaml` (e.g.
  `actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0  # v7.0.0`,
  `actions/setup-go@d35c59abb061a4a6fb18e82ac0862c26744d6ab5  # v5`).
- `make test` already runs `go test ./...` and `make check-fmt`/`make lint`
  already cover all non-`gen/` Go files repo-wide — no changes needed to
  those Makefile targets or to `.github/workflows/lfx-v2-campaign-service-build.yaml`
  for the new packages to be tested/linted in CI.
- Bundle root is `docs/knowledge`.

---

### Task 1: `internal/okf` — frontmatter render/parse + concept writer

**Files:**
- Create: `internal/okf/frontmatter.go`
- Create: `internal/okf/frontmatter_test.go`
- Modify: `go.mod`, `go.sum` (add `gopkg.in/yaml.v3`)

**Interfaces:**
- Produces: `okf.Frontmatter{Type, Title, Description, Resource string}`,
  `(fm Frontmatter) Render() string`, `okf.ParseFrontmatter(data []byte) (map[string]any, string, error)`,
  `okf.WriteConcept(path string, fm Frontmatter, body string) error`. These
  are used by every generator in Tasks 2-6 and by the validator in Task 8.

- [ ] **Step 1: Add the yaml dependency**

Run: `go get gopkg.in/yaml.v3`
Expected: `go.mod` gains a `require gopkg.in/yaml.v3 vX.Y.Z` line and `go.sum` is updated.

- [ ] **Step 2: Write the failing test**

Create `internal/okf/frontmatter_test.go`:

```go
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
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/okf/... -v`
Expected: FAIL — `internal/okf` package does not exist yet (build error: no Go files).

- [ ] **Step 4: Write the implementation**

Create `internal/okf/frontmatter.go`:

```go
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
	s := string(data)
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/okf/... -v`
Expected: PASS (all 5 tests).

- [ ] **Step 6: Format and commit**

Run: `gofmt -s -w internal/okf/frontmatter.go internal/okf/frontmatter_test.go`

```bash
git add internal/okf/frontmatter.go internal/okf/frontmatter_test.go go.mod go.sum
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): add frontmatter render/parse and concept writer

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 2: `internal/okfgen` — docs wrapper generator

**Files:**
- Create: `internal/okfgen/docs.go`
- Create: `internal/okfgen/docs_test.go`

**Interfaces:**
- Consumes: `okf.Frontmatter`, `okf.WriteConcept` (Task 1).
- Produces: `okfgen.ConceptRef{Title, Description, FileName string}`,
  `okfgen.GenerateDocsConcepts(srcDir, destDir string) ([]ConceptRef, error)`,
  and package-private helpers `extractTitleAndDescription(content string) (title, description string)`
  and `firstSentence(s string) string` — reused by Tasks 5 (specs) and 6 (index descriptions).
  `ConceptRef` and `firstSentence`/`extractTitleAndDescription` are relied on by Tasks 3-6.

- [ ] **Step 1: Write the failing test**

Create `internal/okfgen/docs_test.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDocsConcepts(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()

	content := "# Example Doc\n\nThis is the summary sentence. More detail follows.\n"
	if err := os.WriteFile(filepath.Join(srcDir, "example.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	refs, err := GenerateDocsConcepts(srcDir, destDir)
	if err != nil {
		t.Fatalf("GenerateDocsConcepts: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if refs[0].Title != "Example Doc" {
		t.Errorf("Title = %q, want %q", refs[0].Title, "Example Doc")
	}
	if refs[0].Description != "This is the summary sentence." {
		t.Errorf("Description = %q, want %q", refs[0].Description, "This is the summary sentence.")
	}
	if refs[0].FileName != "example.md" {
		t.Errorf("FileName = %q, want %q", refs[0].FileName, "example.md")
	}

	out, err := os.ReadFile(filepath.Join(destDir, "example.md"))
	if err != nil {
		t.Fatalf("reading generated concept: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `type: "Architecture Doc"`) {
		t.Errorf("generated concept missing type frontmatter:\n%s", got)
	}
	if !strings.Contains(got, "This is the summary sentence.") {
		t.Errorf("generated concept missing description:\n%s", got)
	}
}

func TestGenerateDocsConceptsRenamesArchitecture(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()
	content := "# Arch\n\nSummary.\n"
	if err := os.WriteFile(filepath.Join(srcDir, "architecture.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := GenerateDocsConcepts(srcDir, destDir); err != nil {
		t.Fatalf("GenerateDocsConcepts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "overview.md")); err != nil {
		t.Errorf("expected overview.md to exist: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/okfgen/... -v`
Expected: FAIL — `internal/okfgen` package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/okfgen/docs.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/okfgen/... -v`
Expected: PASS (both tests).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w internal/okfgen/docs.go internal/okfgen/docs_test.go`

```bash
git add internal/okfgen/docs.go internal/okfgen/docs_test.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): generate architecture-doc concepts from docs/*.md

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 3: `internal/okfgen` — Kubernetes wrapper generator

**Files:**
- Create: `internal/okfgen/kubernetes.go`
- Create: `internal/okfgen/kubernetes_test.go`

**Interfaces:**
- Consumes: `okf.Frontmatter`, `okf.WriteConcept` (Task 1); `ConceptRef` (Task 2).
- Produces: `okfgen.GenerateKubernetesConcepts(srcDir, destDir string) ([]ConceptRef, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/okfgen/kubernetes_test.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateKubernetesConcepts(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()

	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: example\n"
	if err := os.WriteFile(filepath.Join(srcDir, "deployment.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	refs, err := GenerateKubernetesConcepts(srcDir, destDir)
	if err != nil {
		t.Fatalf("GenerateKubernetesConcepts: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if refs[0].Title != "Deployment" {
		t.Errorf("Title = %q, want %q", refs[0].Title, "Deployment")
	}
	if refs[0].FileName != "deployment.md" {
		t.Errorf("FileName = %q, want %q", refs[0].FileName, "deployment.md")
	}

	out, err := os.ReadFile(filepath.Join(destDir, "deployment.md"))
	if err != nil {
		t.Fatalf("reading generated concept: %v", err)
	}
	if !strings.Contains(string(out), `type: "Kubernetes Resource"`) {
		t.Errorf("missing type frontmatter:\n%s", out)
	}
}

func TestGenerateKubernetesConceptsIgnoresNestedKind(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()

	// The top-level "kind" is HTTPRoute; nested refs also use "kind" but
	// are indented and must not be picked up as the resource's own kind.
	content := "apiVersion: gateway.networking.k8s.io/v1\n" +
		"kind: HTTPRoute\n" +
		"spec:\n" +
		"  parentRefs:\n" +
		"    - kind: Gateway\n"
	if err := os.WriteFile(filepath.Join(srcDir, "httproute.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	refs, err := GenerateKubernetesConcepts(srcDir, destDir)
	if err != nil {
		t.Fatalf("GenerateKubernetesConcepts: %v", err)
	}
	if len(refs) != 1 || refs[0].Title != "HTTPRoute" {
		t.Fatalf("got refs %+v, want a single HTTPRoute ref", refs)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/okfgen/... -run TestGenerateKubernetesConcepts -v`
Expected: FAIL — `GenerateKubernetesConcepts` is undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/okfgen/kubernetes.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

// kindPattern matches an unindented "kind: <value>" line, i.e. the
// resource's own kind rather than a nested reference (e.g. a Gateway
// parentRef inside an HTTPRoute).
var kindPattern = regexp.MustCompile(`(?m)^kind:\s*(\S+)\s*$`)

// GenerateKubernetesConcepts wraps every *.yaml file directly under srcDir
// (a Helm templates directory) into an OKF concept file under destDir.
// Helm templates are not valid standalone YAML (they contain "{{ }}"
// interpolation), so the resource's Kubernetes "kind" is extracted with a
// regular expression rather than a YAML parse.
func GenerateKubernetesConcepts(srcDir, destDir string) ([]ConceptRef, error) {
	matches, err := filepath.Glob(filepath.Join(srcDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", srcDir, err)
	}

	var refs []ConceptRef
	for _, src := range matches {
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", src, err)
		}

		kind := "Unknown"
		if m := kindPattern.FindStringSubmatch(string(data)); m != nil {
			kind = m[1]
		}

		destName := strings.TrimSuffix(filepath.Base(src), ".yaml") + ".md"
		destPath := filepath.Join(destDir, destName)

		relLink, err := filepath.Rel(destDir, src)
		if err != nil {
			return nil, fmt.Errorf("computing relative link for %s: %w", src, err)
		}

		description := fmt.Sprintf("Kubernetes %s manifest for the campaign service, defined in the Helm chart.", kind)
		fm := okf.Frontmatter{
			Type:        "Kubernetes Resource",
			Title:       kind,
			Description: description,
			Resource:    filepath.ToSlash(src),
		}
		body := fmt.Sprintf("# %s\n\n%s\n\nSee [%s](%s).\n",
			kind, description, filepath.ToSlash(src), filepath.ToSlash(relLink))

		if err := okf.WriteConcept(destPath, fm, body); err != nil {
			return nil, err
		}

		refs = append(refs, ConceptRef{Title: kind, Description: description, FileName: destName})
	}
	return refs, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/okfgen/... -v`
Expected: PASS (all tests in the package, including Task 2's).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w internal/okfgen/kubernetes.go internal/okfgen/kubernetes_test.go`

```bash
git add internal/okfgen/kubernetes.go internal/okfgen/kubernetes_test.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): generate kubernetes-resource concepts from Helm templates

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 4: `internal/okfgen` — Go package wrapper generator

**Files:**
- Create: `internal/okfgen/gopackages.go`
- Create: `internal/okfgen/gopackages_test.go`

**Interfaces:**
- Consumes: `okf.Frontmatter`, `okf.WriteConcept` (Task 1); `ConceptRef`, `firstSentence` (Task 2).
- Produces: `okfgen.GenerateGoPackageConcepts(pkgDirs []string, destDir string) ([]ConceptRef, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/okfgen/gopackages_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/okfgen/... -run 'TestExtractPackageDoc|TestGenerateGoPackageConcepts' -v`
Expected: FAIL — `extractPackageDoc`/`GenerateGoPackageConcepts` are undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/okfgen/gopackages.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

// GenerateGoPackageConcepts wraps each directory in pkgDirs (e.g.
// "internal/service") into an OKF concept file under destDir, using each
// package's doc comment as the description and body.
func GenerateGoPackageConcepts(pkgDirs []string, destDir string) ([]ConceptRef, error) {
	var refs []ConceptRef
	for _, dir := range pkgDirs {
		_, doc, err := extractPackageDoc(dir)
		if err != nil {
			return nil, err
		}

		description := doc
		if description == "" {
			description = fmt.Sprintf("Package at %s.", dir)
		} else {
			description = firstSentence(strings.ReplaceAll(description, "\n", " "))
		}

		bodyDoc := doc
		if bodyDoc == "" {
			bodyDoc = description
		}

		destName := strings.ReplaceAll(dir, "/", "-") + ".md"
		destPath := filepath.Join(destDir, destName)

		relLink, err := filepath.Rel(destDir, dir)
		if err != nil {
			return nil, fmt.Errorf("computing relative link for %s: %w", dir, err)
		}

		fm := okf.Frontmatter{
			Type:        "Go Package",
			Title:       dir,
			Description: description,
			Resource:    filepath.ToSlash(dir),
		}
		body := fmt.Sprintf("# %s\n\n%s\n\nSee [%s](%s).\n",
			dir, bodyDoc, filepath.ToSlash(dir), filepath.ToSlash(relLink))

		if err := okf.WriteConcept(destPath, fm, body); err != nil {
			return nil, err
		}

		refs = append(refs, ConceptRef{Title: dir, Description: description, FileName: destName})
	}
	return refs, nil
}

// extractPackageDoc returns the package name and doc comment (the comment
// block immediately preceding the "package" clause, if any, per Go's doc
// comment convention) for the Go package in dir. It ignores _test.go files.
func extractPackageDoc(dir string) (pkgName, doc string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", dir, err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", "", fmt.Errorf("reading %s: %w", name, err)
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, data, parser.ParseComments)
		if err != nil {
			return "", "", fmt.Errorf("parsing %s: %w", name, err)
		}

		if pkgName == "" {
			pkgName = f.Name.Name
		}
		if f.Doc != nil && strings.TrimSpace(f.Doc.Text()) != "" {
			return f.Name.Name, strings.TrimSpace(f.Doc.Text()), nil
		}
	}

	if pkgName == "" {
		return "", "", fmt.Errorf("no go files found in %s", dir)
	}
	return pkgName, "", nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/okfgen/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w internal/okfgen/gopackages.go internal/okfgen/gopackages_test.go`

```bash
git add internal/okfgen/gopackages.go internal/okfgen/gopackages_test.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): generate go-package concepts from package doc comments

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 5: `internal/okfgen` — speckit spec wrapper generator

**Files:**
- Create: `internal/okfgen/specs.go`
- Create: `internal/okfgen/specs_test.go`

**Interfaces:**
- Consumes: `okf.Frontmatter`, `okf.WriteConcept` (Task 1); `ConceptRef`, `extractTitleAndDescription` (Task 2).
- Produces: `okfgen.GenerateSpecConcepts(srcFiles []string, destDir string) ([]ConceptRef, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/okfgen/specs_test.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSpecConcepts(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.md")
	content := "# Health Endpoints — Spec\n\nDefines liveness and readiness endpoints. More detail follows.\n"
	if err := os.WriteFile(specPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	destDir := filepath.Join(dir, "knowledge", "specs")
	refs, err := GenerateSpecConcepts([]string{specPath}, destDir)
	if err != nil {
		t.Fatalf("GenerateSpecConcepts: %v", err)
	}
	if len(refs) != 1 || refs[0].Title != "Health Endpoints — Spec" {
		t.Fatalf("unexpected refs: %+v", refs)
	}
	if refs[0].Description != "Defines liveness and readiness endpoints." {
		t.Errorf("Description = %q", refs[0].Description)
	}

	if _, err := os.Stat(filepath.Join(destDir, "spec.md")); err != nil {
		t.Errorf("expected spec.md to exist: %v", err)
	}
}

func TestGenerateSpecConceptsFallsBackToFileName(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(specPath, []byte("No heading here.\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	destDir := filepath.Join(dir, "knowledge", "specs")
	refs, err := GenerateSpecConcepts([]string{specPath}, destDir)
	if err != nil {
		t.Fatalf("GenerateSpecConcepts: %v", err)
	}
	if refs[0].Title != "notes.md" {
		t.Errorf("Title = %q, want fallback %q", refs[0].Title, "notes.md")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/okfgen/... -run TestGenerateSpecConcepts -v`
Expected: FAIL — `GenerateSpecConcepts` is undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/okfgen/specs.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okf"
)

// GenerateSpecConcepts wraps each file in srcFiles (e.g.
// "specs/001-health-endpoints/spec.md") into an OKF concept file under
// destDir.
func GenerateSpecConcepts(srcFiles []string, destDir string) ([]ConceptRef, error) {
	var refs []ConceptRef
	for _, src := range srcFiles {
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", src, err)
		}

		title, description := extractTitleAndDescription(string(data))
		if title == "" {
			title = filepath.Base(src)
		}

		destName := filepath.Base(src)
		destPath := filepath.Join(destDir, destName)

		relLink, err := filepath.Rel(destDir, src)
		if err != nil {
			return nil, fmt.Errorf("computing relative link for %s: %w", src, err)
		}

		fm := okf.Frontmatter{
			Type:        "Feature Spec",
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/okfgen/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w internal/okfgen/specs.go internal/okfgen/specs_test.go`

```bash
git add internal/okfgen/specs.go internal/okfgen/specs_test.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): generate feature-spec concepts from speckit docs

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 6: `internal/okfgen` — index.md and log.md writers

**Files:**
- Create: `internal/okfgen/index.go`
- Create: `internal/okfgen/index_test.go`
- Create: `internal/okfgen/log.go`
- Create: `internal/okfgen/log_test.go`

**Interfaces:**
- Consumes: `ConceptRef` (Task 2).
- Produces: `okfgen.IndexEntry{Title, Link, Description string}`,
  `okfgen.RenderIndex(title string, entries []IndexEntry) string`,
  `okfgen.WriteIndex(path, title string, entries []IndexEntry) error`,
  `okfgen.EntriesFromRefs(refs []ConceptRef) []IndexEntry`,
  `okfgen.SeedLog(destDir, date, message string) error`. Used by
  `cmd/okfgen/main.go` in Task 7.

- [ ] **Step 1: Write the failing tests**

Create `internal/okfgen/index_test.go`:

```go
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
```

Create `internal/okfgen/log_test.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedLog(t *testing.T) {
	dir := t.TempDir()
	if err := SeedLog(dir, "2026-07-09", "initial bundle generated."); err != nil {
		t.Fatalf("SeedLog: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "log.md"))
	if err != nil {
		t.Fatalf("reading log.md: %v", err)
	}
	want := "# Log\n\n## 2026-07-09\n\n**Creation** — initial bundle generated.\n"
	if string(data) != want {
		t.Errorf("log.md = %q, want %q", data, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/okfgen/... -run 'TestRenderIndex|TestWriteIndex|TestEntriesFromRefs|TestSeedLog' -v`
Expected: FAIL — `RenderIndex`/`WriteIndex`/`EntriesFromRefs`/`SeedLog` are undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/okfgen/index.go`:

```go
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
```

Create `internal/okfgen/log.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"fmt"
	"os"
	"path/filepath"
)

// SeedLog writes an initial log.md at destDir/log.md with one dated entry,
// per OKF §7 (log.md has no frontmatter, "##"-level ISO 8601 date headings,
// newest first).
func SeedLog(destDir, date, message string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", destDir, err)
	}
	path := filepath.Join(destDir, "log.md")
	content := fmt.Sprintf("# Log\n\n## %s\n\n**Creation** — %s\n", date, message)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/okfgen/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w internal/okfgen/index.go internal/okfgen/index_test.go internal/okfgen/log.go internal/okfgen/log_test.go`

```bash
git add internal/okfgen/index.go internal/okfgen/index_test.go internal/okfgen/log.go internal/okfgen/log_test.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): add index.md and log.md writers

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 7: `cmd/okfgen` — orchestrator + generate the real bundle

**Files:**
- Create: `cmd/okfgen/main.go`
- Create: `docs/knowledge/**` (generated output, committed as real files)

**Interfaces:**
- Consumes: everything produced in Tasks 1-6.
- Produces: the on-disk `docs/knowledge/` bundle consumed by Task 9's validator run and referenced by Task 11's `CLAUDE.md`.

- [ ] **Step 1: Write `cmd/okfgen/main.go`**

Create `cmd/okfgen/main.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Command okfgen bootstraps the initial OKF knowledge bundle under
// docs/knowledge/ from this repo's existing docs, Helm chart templates, Go
// packages, and speckit specs.
//
// It is meant to be run once (or deliberately re-run to bootstrap a new
// subtree) from the repo root:
//
//	go run ./cmd/okfgen
//
// Re-running overwrites every concept and index file it manages, so it
// will clobber any manual edits made to those files afterward. Day-to-day
// maintenance of the knowledge bundle is done by hand-editing concept
// files directly (see the "Knowledge Base (OKF)" section of README.md) —
// not by re-running this tool.
package main

import (
	"fmt"
	"os"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okfgen"
)

const bundleRoot = "docs/knowledge"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "okfgen:", err)
		os.Exit(1)
	}
}

func run() error {
	archRefs, err := okfgen.GenerateDocsConcepts("docs", bundleRoot+"/architecture")
	if err != nil {
		return err
	}
	if err := okfgen.WriteIndex(bundleRoot+"/architecture/index.md", "Architecture",
		okfgen.EntriesFromRefs(archRefs)); err != nil {
		return err
	}

	k8sRefs, err := okfgen.GenerateKubernetesConcepts(
		"charts/lfx-v2-campaign-service/templates", bundleRoot+"/kubernetes")
	if err != nil {
		return err
	}
	if err := okfgen.WriteIndex(bundleRoot+"/kubernetes/index.md", "Kubernetes",
		okfgen.EntriesFromRefs(k8sRefs)); err != nil {
		return err
	}

	pkgDirs := []string{
		"cmd/campaign-service",
		"internal/container",
		"internal/infrastructure/config",
		"internal/middleware",
		"internal/service",
		"pkg/constants",
		"pkg/log",
		"pkg/utils",
		"design",
	}
	codeRefs, err := okfgen.GenerateGoPackageConcepts(pkgDirs, bundleRoot+"/code")
	if err != nil {
		return err
	}
	if err := okfgen.WriteIndex(bundleRoot+"/code/index.md", "Code",
		okfgen.EntriesFromRefs(codeRefs)); err != nil {
		return err
	}

	specFiles := []string{
		"specs/001-health-endpoints/spec.md",
		"specs/001-health-endpoints/plan.md",
		"specs/001-health-endpoints/tasks.md",
		"specs/001-health-endpoints/data-model.md",
		"specs/001-health-endpoints/quickstart.md",
		"specs/001-health-endpoints/research.md",
	}
	specRefs, err := okfgen.GenerateSpecConcepts(specFiles, bundleRoot+"/specs/001-health-endpoints")
	if err != nil {
		return err
	}
	if err := okfgen.WriteIndex(bundleRoot+"/specs/001-health-endpoints/index.md",
		"001 Health Endpoints", okfgen.EntriesFromRefs(specRefs)); err != nil {
		return err
	}
	if err := okfgen.WriteIndex(bundleRoot+"/specs/index.md", "Specs", []okfgen.IndexEntry{
		{
			Title:       "001 Health Endpoints",
			Link:        "001-health-endpoints/index.md",
			Description: "Feature spec, plan, and tasks for the liveness/readiness health endpoints.",
		},
	}); err != nil {
		return err
	}

	if err := okfgen.WriteIndex(bundleRoot+"/index.md", "Knowledge Base", []okfgen.IndexEntry{
		{Title: "Architecture", Link: "architecture/index.md",
			Description: "Architecture, API catalog, and data schema docs for the campaign service."},
		{Title: "Kubernetes", Link: "kubernetes/index.md",
			Description: "Kubernetes resources defined in the Helm chart."},
		{Title: "Code", Link: "code/index.md",
			Description: "Go package structure of the service."},
		{Title: "Specs", Link: "specs/index.md",
			Description: "Feature specs tracked via speckit."},
	}); err != nil {
		return err
	}

	return okfgen.SeedLog(bundleRoot, "2026-07-09",
		"initial OKF knowledge bundle generated from existing docs, Helm charts, Go packages, and speckit specs.")
}
```

- [ ] **Step 2: Build it**

Run: `go build ./cmd/okfgen/...`
Expected: builds with no errors.

- [ ] **Step 3: Run it to generate the bundle**

Run: `go run ./cmd/okfgen`
Expected: exits 0 with no output; `docs/knowledge/` now exists with `index.md`, `log.md`, and the `architecture/`, `kubernetes/`, `code/`, `specs/` subtrees.

- [ ] **Step 4: Inspect the generated tree**

Run: `find docs/knowledge -type f | sort`
Expected output (paths, not contents):
```
docs/knowledge/architecture/api-catalog.md
docs/knowledge/architecture/build-summary.md
docs/knowledge/architecture/channel-connections-schema.md
docs/knowledge/architecture/index.md
docs/knowledge/architecture/overview.md
docs/knowledge/code/cmd-campaign-service.md
docs/knowledge/code/design.md
docs/knowledge/code/index.md
docs/knowledge/code/internal-container.md
docs/knowledge/code/internal-infrastructure-config.md
docs/knowledge/code/internal-middleware.md
docs/knowledge/code/internal-service.md
docs/knowledge/code/pkg-constants.md
docs/knowledge/code/pkg-log.md
docs/knowledge/code/pkg-utils.md
docs/knowledge/index.md
docs/knowledge/kubernetes/deployment.md
docs/knowledge/kubernetes/heimdall-middleware.md
docs/knowledge/kubernetes/httproute.md
docs/knowledge/kubernetes/index.md
docs/knowledge/kubernetes/pdb.md
docs/knowledge/kubernetes/ruleset.md
docs/knowledge/kubernetes/service.md
docs/knowledge/kubernetes/serviceaccount.md
docs/knowledge/log.md
docs/knowledge/specs/001-health-endpoints/data-model.md
docs/knowledge/specs/001-health-endpoints/index.md
docs/knowledge/specs/001-health-endpoints/plan.md
docs/knowledge/specs/001-health-endpoints/quickstart.md
docs/knowledge/specs/001-health-endpoints/research.md
docs/knowledge/specs/001-health-endpoints/spec.md
docs/knowledge/specs/001-health-endpoints/tasks.md
docs/knowledge/specs/index.md
```
Spot-check a couple of files with `cat` (e.g. `docs/knowledge/architecture/overview.md`, `docs/knowledge/kubernetes/deployment.md`) to confirm frontmatter and links look right. If any title/description reads oddly, fix it by hand now (the file is committed as-is going forward; this is the one time the generator's output gets human review before becoming the canonical copy).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w cmd/okfgen/main.go`

```bash
git add cmd/okfgen/main.go docs/knowledge
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): generate initial docs/knowledge bundle

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 8: `internal/okfvalidate` — OKF §9 conformance checker

**Files:**
- Create: `internal/okfvalidate/validate.go`
- Create: `internal/okfvalidate/validate_test.go`

**Interfaces:**
- Consumes: `okf.Frontmatter`, `okf.ParseFrontmatter`, `okf.WriteConcept` (Task 1).
- Produces: `okfvalidate.Validate(bundleDir string) []error`, used by `cmd/okfvalidate/main.go` in Task 9.

- [ ] **Step 1: Write the failing tests**

Create `internal/okfvalidate/validate_test.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfvalidate

import (
	"os"
	"path/filepath"
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

func TestValidateLogNotSorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "log.md"),
		"# Log\n\n## 2026-01-01\n\n**Update** — old.\n\n## 2026-07-09\n\n**Update** — new.\n")

	if errs := Validate(dir); len(errs) == 0 {
		t.Fatal("Validate() = no errors, want an unsorted-log error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/okfvalidate/... -v`
Expected: FAIL — `internal/okfvalidate` package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/okfvalidate/validate.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package okfvalidate checks an Open Knowledge Format (OKF) v0.1 bundle for
// conformance per OKF SPEC.md §9: every non-reserved .md file has a
// parseable frontmatter block with a non-empty "type", index.md files
// carry no frontmatter (except an optional okf_version at the bundle root)
// and use the "* [Title](url) - description" bullet form, and log.md uses
// "##"-level ISO 8601 date headings sorted newest first.
package okfvalidate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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
	content := string(data)

	if strings.HasPrefix(content, "---\n") {
		fm, body, err := okf.ParseFrontmatter(data)
		if err != nil {
			return []error{fmt.Errorf("%s: %w", path, err)}
		}
		allowed := map[string]bool{}
		if isRoot {
			allowed["okf_version"] = true
		}
		var errs []error
		for k := range fm {
			if !allowed[k] {
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/okfvalidate/... -v`
Expected: PASS (all 6 tests).

- [ ] **Step 5: Format and commit**

Run: `gofmt -s -w internal/okfvalidate/validate.go internal/okfvalidate/validate_test.go`

```bash
git add internal/okfvalidate/validate.go internal/okfvalidate/validate_test.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): add OKF §9 bundle conformance validator

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 9: `cmd/okfvalidate` — CLI + validate the real bundle

**Files:**
- Create: `cmd/okfvalidate/main.go`

**Interfaces:**
- Consumes: `okfvalidate.Validate` (Task 8).

- [ ] **Step 1: Write `cmd/okfvalidate/main.go`**

Create `cmd/okfvalidate/main.go`:

```go
// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Command okfvalidate checks an Open Knowledge Format bundle for v0.1
// conformance (see internal/okfvalidate). Usage:
//
//	go run ./cmd/okfvalidate [bundle-dir]
//
// bundle-dir defaults to docs/knowledge.
package main

import (
	"fmt"
	"os"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/okfvalidate"
)

func main() {
	bundleDir := "docs/knowledge"
	if len(os.Args) > 1 {
		bundleDir = os.Args[1]
	}

	errs := okfvalidate.Validate(bundleDir)
	if len(errs) == 0 {
		fmt.Println("okfvalidate: bundle is OKF-conformant:", bundleDir)
		return
	}

	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "okfvalidate:", e)
	}
	os.Exit(1)
}
```

- [ ] **Step 2: Build it**

Run: `go build ./cmd/okfvalidate/...`
Expected: builds with no errors.

- [ ] **Step 3: Run it against the real bundle generated in Task 7**

Run: `go run ./cmd/okfvalidate ./docs/knowledge`
Expected: `okfvalidate: bundle is OKF-conformant: ./docs/knowledge` and exit code 0.

If it reports errors, fix the flagged concept/index/log files under
`docs/knowledge/` (by hand, or by adjusting the Task 2-7 generators and
re-running `go run ./cmd/okfgen` if the bug is systemic) until this passes.

- [ ] **Step 4: Format and commit**

Run: `gofmt -s -w cmd/okfvalidate/main.go`

```bash
git add cmd/okfvalidate/main.go
git commit -S --signoff -m "$(cat <<'EOF'
feat(okf): add okfvalidate CLI

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 10: CI workflow — `validate-okf.yml`

**Files:**
- Create: `.github/workflows/validate-okf.yml`

**Interfaces:**
- Consumes: `go run ./cmd/okfvalidate` (Task 9).

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/validate-okf.yml`:

```yaml
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT

name: Validate OKF Knowledge Bundle

"on":
  pull_request:
    paths:
      - "docs/knowledge/**"
      - "cmd/okfvalidate/**"
      - "internal/okfvalidate/**"
      - "internal/okf/**"

permissions:
  contents: read

jobs:
  validate-okf:
    name: Validate OKF Bundle
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0  # v7.0.0

      - name: Setup Go
        uses: actions/setup-go@d35c59abb061a4a6fb18e82ac0862c26744d6ab5  # v5
        with:
          go-version-file: go.mod

      - name: Validate docs/knowledge
        run: go run ./cmd/okfvalidate ./docs/knowledge
```

- [ ] **Step 2: Sanity-check the workflow locally**

Run: `go run ./cmd/okfvalidate ./docs/knowledge`
Expected: `okfvalidate: bundle is OKF-conformant: ./docs/knowledge` (same command the workflow runs).

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/validate-okf.yml
git commit -S --signoff -m "$(cat <<'EOF'
ci: validate the OKF knowledge bundle on PRs

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 11: `CLAUDE.md` rewrite + `AGENTS.md` symlink

**Files:**
- Modify: `CLAUDE.md`
- Create: `AGENTS.md` (symlink to `CLAUDE.md`)

- [ ] **Step 1: Rewrite `CLAUDE.md`**

Replace the full contents of `CLAUDE.md` with:

```markdown
# LFX V2 Campaign Service — Agent Guide

Backend service for LFX Self Serve marketing campaign operations: a Go/Goa
HTTP API deployed via Helm, brokering between the LFX UI and paid
advertising platforms.

## Start here

Before reading source files directly, consult
[`docs/knowledge/index.md`](docs/knowledge/index.md) — an
[Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle that maps this repo's architecture docs, Kubernetes resources, Go
packages, and feature specs without requiring the whole repo in context.

## Keep the knowledge base current

Whenever you merge a PR, update a Helm manifest, or fix a bug:

1. Update the relevant concept file(s) under `docs/knowledge/**` (add a new
   one with OKF frontmatter — `type`, `title`, `description` — if no
   existing concept covers the change).
2. Update the containing `index.md` bullet if a concept was added, renamed,
   or its description changed.
3. Append a dated entry to `docs/knowledge/log.md`
   (`## YYYY-MM-DD` / `**Update** — ...`).
4. Validate locally: `go run ./cmd/okfvalidate ./docs/knowledge`.

Do not re-run `go run ./cmd/okfgen` to do this — it regenerates the entire
bundle from source and will clobber hand-edited concept files. It exists
only to bootstrap new subtrees.

## Active feature spec

The current active speckit feature spec/plan/tasks live under
[`specs/001-health-endpoints/`](specs/001-health-endpoints/plan.md).

## Development

See `README.md` for the `make` targets used to build, test, lint, and run
the service.
```

- [ ] **Step 2: Symlink `AGENTS.md` to `CLAUDE.md`**

Run: `ln -s CLAUDE.md AGENTS.md`
Expected: `AGENTS.md` is created as a symlink; `cat AGENTS.md` prints the same content as `CLAUDE.md`.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md AGENTS.md
git commit -S --signoff -m "$(cat <<'EOF'
docs: rewrite CLAUDE.md for OKF knowledge bundle, symlink AGENTS.md

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

### Task 12: `README.md` — Knowledge Base (OKF) maintenance section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add the section**

In `README.md`, insert a new `## Knowledge Base (OKF)` section directly
after the existing `## Development` section (i.e. at the end of the file):

```markdown

## Knowledge Base (OKF)

`docs/knowledge/` is an [Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle — plain markdown with YAML frontmatter — that gives humans and AI
agents a structured map of this repo's architecture, Kubernetes resources,
Go packages, and feature specs. Start at
[`docs/knowledge/index.md`](docs/knowledge/index.md).

**When to update it:** after merging a feature PR, changing an API
endpoint, adding or modifying a Helm resource, or changing a package's
responsibility.

**How to update it:**

1. Edit the relevant existing concept file under `docs/knowledge/**`, or add
   a new one with OKF frontmatter (`type`, `title`, `description`) if no
   existing concept covers the change. Do **not** regenerate with
   `go run ./cmd/okfgen` — that tool bootstraps new subtrees and will
   overwrite hand-edited concept files.
2. Add or update the concept's `* [Title](url) - description` bullet in the
   relevant `index.md`.
3. Append a dated entry to `docs/knowledge/log.md`:
   `## YYYY-MM-DD` followed by `**Update** — <what changed and why>.`

**Validate before pushing:**

```sh
go run ./cmd/okfvalidate ./docs/knowledge
```

This is the same check `.github/workflows/validate-okf.yml` runs in CI.

Agents are expected to do this bookkeeping automatically (see `CLAUDE.md`);
developers making manual changes should follow the same convention.
```

- [ ] **Step 2: Lint the markdown**

Run: `markdownlint README.md`
Expected: no errors (fix any it reports — e.g. heading levels, line length —
before committing).

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -S --signoff -m "$(cat <<'EOF'
docs: add Knowledge Base (OKF) maintenance section to README

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: David Deal <ddeal@linuxfoundation.org>
EOF
)"
```

---

## Final verification

After Task 12, run the full repo check suite once to confirm nothing broke:

```bash
make check-fmt
make lint
make test
go run ./cmd/okfvalidate ./docs/knowledge
```

All four must pass before considering this plan complete.

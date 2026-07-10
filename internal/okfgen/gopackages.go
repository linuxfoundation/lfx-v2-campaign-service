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

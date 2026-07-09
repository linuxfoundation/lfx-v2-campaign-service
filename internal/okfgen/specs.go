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

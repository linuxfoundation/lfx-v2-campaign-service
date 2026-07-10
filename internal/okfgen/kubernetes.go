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

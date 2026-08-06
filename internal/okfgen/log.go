// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package okfgen

import (
	"fmt"
	"os"
	"path/filepath"
)

// SeedLog writes an initial log fragment at destDir/log/<date>-bundle-created.md
// with one dated entry: no frontmatter (a fragment is not a concept), and a
// first H1 carrying the same date as the filename.
func SeedLog(destDir, date, message string) error {
	logDir := filepath.Join(destDir, "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", logDir, err)
	}
	path := filepath.Join(logDir, date+"-bundle-created.md")
	content := fmt.Sprintf("# %s — bundle created\n\n**Creation** — %s\n", date, message)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

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

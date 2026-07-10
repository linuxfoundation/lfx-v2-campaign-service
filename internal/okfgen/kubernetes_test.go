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

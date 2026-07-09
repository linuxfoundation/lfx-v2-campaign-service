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
		return fmt.Errorf("generate architecture concepts: %w", err)
	}
	if err := okfgen.WriteIndex(bundleRoot+"/architecture/index.md", "Architecture",
		okfgen.EntriesFromRefs(archRefs)); err != nil {
		return fmt.Errorf("write architecture index: %w", err)
	}

	k8sRefs, err := okfgen.GenerateKubernetesConcepts(
		"charts/lfx-v2-campaign-service/templates", bundleRoot+"/kubernetes")
	if err != nil {
		return fmt.Errorf("generate kubernetes concepts: %w", err)
	}
	if err := okfgen.WriteIndex(bundleRoot+"/kubernetes/index.md", "Kubernetes",
		okfgen.EntriesFromRefs(k8sRefs)); err != nil {
		return fmt.Errorf("write kubernetes index: %w", err)
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
		return fmt.Errorf("generate code concepts: %w", err)
	}
	if err := okfgen.WriteIndex(bundleRoot+"/code/index.md", "Code",
		okfgen.EntriesFromRefs(codeRefs)); err != nil {
		return fmt.Errorf("write code index: %w", err)
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
		return fmt.Errorf("generate spec concepts: %w", err)
	}
	if err := okfgen.WriteIndex(bundleRoot+"/specs/001-health-endpoints/index.md",
		"001 Health Endpoints", okfgen.EntriesFromRefs(specRefs)); err != nil {
		return fmt.Errorf("write spec index: %w", err)
	}
	if err := okfgen.WriteIndex(bundleRoot+"/specs/index.md", "Specs", []okfgen.IndexEntry{
		{
			Title:       "001 Health Endpoints",
			Link:        "001-health-endpoints/index.md",
			Description: "Feature spec, plan, and tasks for the liveness/readiness health endpoints.",
		},
	}); err != nil {
		return fmt.Errorf("write specs index: %w", err)
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
		return fmt.Errorf("write root index: %w", err)
	}

	if err := okfgen.SeedLog(bundleRoot, "2026-07-09",
		"initial OKF knowledge bundle generated from existing docs, Helm charts, Go packages, and speckit specs."); err != nil {
		return fmt.Errorf("seed log: %w", err)
	}
	return nil
}

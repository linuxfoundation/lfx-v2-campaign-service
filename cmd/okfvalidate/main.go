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

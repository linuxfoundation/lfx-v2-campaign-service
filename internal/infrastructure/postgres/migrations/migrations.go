// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package migrations embeds the SQL migration files so they can be run with
// golang-migrate's iofs source driver from the compiled binary.
package migrations

import "embed"

// FS holds all *.sql migration files at the root of this package.
//
//go:embed *.sql
var FS embed.FS

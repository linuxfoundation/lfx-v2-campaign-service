// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Command sysacct-bootstrap installs the LF-owned system ad-account credentials
// that projects without a connection of their own fall back to.
//
// Usage:
//
//	DATABASE_URL=... CREDENTIAL_ENCRYPTION_KEY=... \
//	  sysacct-bootstrap -provider google-ads [-account-id 8666746580] < creds.json
//
// The credential document is read from STDIN and never from a flag: a flag value
// lands in shell history and in every `ps` listing on the host for the lifetime of
// the process, which for a long-lived refresh token is an indefinite exposure.
//
// This is a separate binary rather than a subcommand of campaign-service so the
// serving path grows no flags, and so the job that runs it can be given database
// and key access without also being able to serve traffic.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/bootstrap"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	// maxCredentialBytes bounds the stdin read. A credential document is a handful of
	// tokens; anything larger is a misdirected pipe, and io.ReadAll on one is unbounded.
	maxCredentialBytes = 64 << 10
	bootstrapTimeout   = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	provider := flag.String("provider", "", "provider to install (e.g. google-ads)")
	accountID := flag.String("account-id", "", "optional ad account id; omit to install credentials first and discover the account afterwards")
	flag.Parse()

	if *provider == "" {
		return fmt.Errorf("-provider is required (one of %v)", model.AllProviders())
	}
	dsn := os.Getenv(constants.EnvDatabaseURL)
	if dsn == "" {
		return fmt.Errorf("%s is not set", constants.EnvDatabaseURL)
	}

	// Read the credential BEFORE opening the database: a malformed or absent
	// document is the likeliest failure and there is no reason to hold a pool
	// open across it. One byte past the limit so an oversized document is
	// detected rather than silently truncated into a half-credential.
	credsJSON, err := io.ReadAll(io.LimitReader(os.Stdin, maxCredentialBytes+1))
	if err != nil {
		return fmt.Errorf("read credentials from stdin: %w", err)
	}
	if len(credsJSON) > maxCredentialBytes {
		return fmt.Errorf("credentials exceed %d bytes; check that stdin is the credential document", maxCredentialBytes)
	}
	if len(credsJSON) == 0 {
		return fmt.Errorf("no credentials on stdin; pipe the provider's credential json in")
	}

	enc, err := crypto.NewAESGCMFromBase64(os.Getenv(constants.EnvCredentialEncryptionKey))
	if err != nil {
		return fmt.Errorf("init credential encryptor: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if err := bootstrap.InstallSystemCredentials(
		ctx, postgres.NewConnectionRepo(pool), enc, model.Provider(*provider), *accountID, credsJSON,
	); err != nil {
		return err
	}
	// The credential itself is never echoed; what an operator needs confirmed is
	// which provider now has a system account, not what was installed.
	fmt.Printf("system account credentials installed for %s\n", *provider)
	return nil
}

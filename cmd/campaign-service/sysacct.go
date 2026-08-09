// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/bootstrap"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	// bootstrapSystemAccountCmd installs the LF-owned credentials unconnected projects fall
	// back to: `campaign-service bootstrap-system-account -provider google-ads
	// [-account-id 8666746580] [-config login_customer_id=999] < creds.json`. A subcommand of
	// the SERVING binary because ko publishes only cmd/campaign-service — a separate binary
	// would have no published artifact for the deployment Job to run.
	bootstrapSystemAccountCmd = "bootstrap-system-account"

	// maxCredentialBytes bounds the stdin read: a credential is a handful of tokens, so
	// anything larger is a misdirected pipe and io.ReadAll on one is unbounded.
	maxCredentialBytes = 64 << 10
	bootstrapTimeout   = 30 * time.Second
)

// parseProviderConfig turns `k=v,k2=v2` into the connection's non-secret config columns — the
// plumbing an adapter reads out of ProviderConfig (LinkedIn's org_id, Meta's page_id, X's
// funding_instrument_id), without which a row fails at campaign create.
func parseProviderConfig(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	cfg := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if k = strings.TrimSpace(k); !ok || k == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("-config entry %q is not key=value", pair)
		}
		cfg[k] = strings.TrimSpace(v)
	}
	return cfg, nil
}

// runSysacctBootstrap is the subcommand entry point. The credential is read from STDIN, never
// a flag: a flag lands in shell history and every `ps` listing, indefinite exposure for a
// long-lived refresh token.
func runSysacctBootstrap(args []string) error {
	fs := flag.NewFlagSet(bootstrapSystemAccountCmd, flag.ContinueOnError)
	provider := fs.String("provider", "", "provider to install (e.g. google-ads)")
	accountID := fs.String("account-id", "", "ad account id; omittable for google-ads only, to install credentials first and discover the account afterwards (every other provider is refused without it: no discovery endpoint exists to finish the row later)")
	configKV := fs.String("config", "", "non-secret provider config as key=value pairs, e.g. org_id=123")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *provider == "" {
		return fmt.Errorf("-provider is required (one of %v)", model.AllProviders())
	}
	cfg, err := parseProviderConfig(*configKV)
	if err != nil {
		return err
	}
	// The SERVER's resolver, not os.Getenv: in-cluster DATABASE_URL is unset.
	dsn, err := config.ResolveDatabaseURL()
	if err != nil {
		return fmt.Errorf("resolve database settings: %w", err)
	}
	if dsn == "" {
		return fmt.Errorf("no database configured; set PGHOST/PGUSER/PGPASSWORD/PGDATABASE or %s", constants.EnvDatabaseURL)
	}

	// Read the credential BEFORE opening the database: a malformed document is the likeliest
	// failure. One byte past the limit so an oversize is detected, not truncated.
	credsJSON, err := io.ReadAll(io.LimitReader(os.Stdin, maxCredentialBytes+1))
	switch {
	case err != nil:
		return fmt.Errorf("read credentials from stdin: %w", err)
	case len(credsJSON) > maxCredentialBytes:
		return fmt.Errorf("credentials exceed %d bytes; check that stdin is the credential document", maxCredentialBytes)
	case len(credsJSON) == 0:
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

	if err := bootstrap.InstallSystemCredentials(ctx, postgres.NewConnectionRepo(pool), enc,
		model.Provider(*provider), *accountID, cfg, credsJSON); err != nil {
		return err
	}
	// Never echo the credential: what needs confirming is which provider now has an account.
	fmt.Printf("system account credentials installed for %s\n", *provider)
	return nil
}

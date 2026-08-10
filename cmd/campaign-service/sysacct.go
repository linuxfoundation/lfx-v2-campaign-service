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

// firstCommand reports the subcommand named by args (os.Args minus the program name), and
// whether args names one at all.
//
// Only args[0] is considered. A subcommand has to come first, and scanning further would
// mistake a FLAG VALUE for a command: `-p 8080` puts a bare `8080` in the argument list that
// belongs to -p, and rejecting it would break ordinary server startup.
func firstCommand(args []string) (string, bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	return args[0], true
}

// runCommand handles the subcommand named by args (os.Args minus the program name), if args
// names one at all. handled reports whether main must stop rather than serve, and code is the
// process exit status when it does.
//
// The decision is returned rather than exited on so it is testable without a process: the case
// that matters is an UNRECOGNISED command, which used to fall through into server startup.
func runCommand(args []string, stderr io.Writer) (handled bool, code int) {
	name, isCmd := firstCommand(args)
	if !isCmd {
		return false, 0
	}
	if name != bootstrapSystemAccountCmd {
		// Writing the diagnostic is best effort: the exit code is what a Job fails on.
		_, _ = fmt.Fprintf(stderr, "unknown command %q; the only subcommand is %s (server flags begin with -)\n",
			name, bootstrapSystemAccountCmd)
		return true, 2
	}
	if err := runSysacctBootstrap(args[1:]); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return true, 1
	}
	return true, 0
}

// parseProviderConfig turns `k=v,k2=v2` into the connection's non-secret config columns — the
// plumbing an adapter reads out of ProviderConfig (LinkedIn's org_id, Meta's page_id, X's
// funding_instrument_id), without which a row fails at campaign create.
//
// `k=` with no value is an explicit CLEAR of that column, not a malformed entry. It has to be
// expressible here because this installer is the only writer the system scope has — HTTP is
// blocked by rejectSystemScope — and the merge in InstallSystemCredentials preserves any key a
// run does not mention. Without a clear, an optional column that has become wrong (a
// login_customer_id for a manager account no longer in the path) could never be removed by
// anything, from anywhere.
func parseProviderConfig(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	cfg := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return nil, fmt.Errorf("-config entry %q is not key=value (use %s= to clear a column)", pair, k)
		}
		cfg[k] = strings.TrimSpace(v)
	}
	return cfg, nil
}

// paidAdsProviders lists the providers this command can actually install, for its usage
// error. Derived by asking IsPaidAds rather than hand-listing, so it stays in step with the
// same classification bootstrap.InstallSystemAccount enforces — a provider added later is
// offered here exactly when it starts being accepted there, never before.
func paidAdsProviders() []model.Provider {
	out := make([]model.Provider, 0, len(model.AllProviders()))
	for _, p := range model.AllProviders() {
		if p.IsPaidAds() {
			out = append(out, p)
		}
	}
	return out
}

// runSysacctBootstrap is the subcommand entry point. The credential is read from STDIN, never
// a flag: a flag lands in shell history and every `ps` listing, indefinite exposure for a
// long-lived refresh token.
func runSysacctBootstrap(args []string) error {
	fs := flag.NewFlagSet(bootstrapSystemAccountCmd, flag.ContinueOnError)
	provider := fs.String("provider", "", "provider to install (e.g. google-ads)")
	accountID := fs.String("account-id", "", "ad account id. On a FIRST install, omitting it is the credentials-first state, and google-ads alone allows it (its dispatcher can discover the account afterwards; every other provider is refused, since nothing could finish the row later). On a rotation it means KEEP the id already on the row — use -clear-account-id to remove one")
	clearAccountID := fs.Bool("clear-account-id", false, "drop the account selection from an existing row, returning it to the credentials-first state; only for a provider with account discovery, and never combined with -account-id")
	configKV := fs.String("config", "", "non-secret provider config as key=value pairs, e.g. org_id=123. Keys not mentioned keep their current value; `key=` with no value clears that column")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// flag.Parse STOPS at the first non-flag argument and leaves the rest in Args(), so a
	// stray word swallows every flag after it without any error. `-provider google-ads typo
	// -account-id 123` would install a credentials-first row here, or on a rotation keep an
	// account id the operator believed they were changing — a silently wrong outcome on the
	// command that installs the credentials paid campaigns are dispatched with. There is no
	// positional argument in this subcommand's grammar, so anything left over is a mistake.
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: this command takes flags only, and Go's flag "+
			"parser IGNORES every flag after the first non-flag word, so re-check the ones you "+
			"passed after it", rest[0])
	}

	if *provider == "" {
		// Only the paid-ads providers, not AllProviders(): a HubSpot row is refused further
		// down (the reserved-scope fallback resolves paid ads only), so offering it here
		// would send an operator to a value that cannot succeed.
		return fmt.Errorf("-provider is required (one of %v)", paidAdsProviders())
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
		model.Provider(*provider), *accountID, *clearAccountID, cfg, credsJSON); err != nil {
		return err
	}
	// Never echo the credential: what needs confirming is which provider now has an account.
	fmt.Printf("system account credentials installed for %s\n", *provider)
	return nil
}

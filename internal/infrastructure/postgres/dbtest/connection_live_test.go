// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// connectionRepo wraps the live pool in the container type the repository takes.
func connectionRepo(pool *pgxpool.Pool) *postgres.ConnectionRepo {
	return postgres.NewConnectionRepo(&postgres.Pool{Pool: pool})
}

// newGoogleAdsConn is the row every test below starts from: a Google Ads connection
// with a credential blob and a login_customer_id, which is the shape the
// bootstrap-system-account rotation writes and re-writes.
func newGoogleAdsConn(projectID, accountID string) *model.Connection {
	return &model.Connection{
		ProjectID:            projectID,
		Provider:             model.ProviderGoogleAds,
		Label:                "live-test",
		AccountID:            accountID,
		EncryptedCredentials: []byte("ciphertext-v1"),
		ProviderConfig:       map[string]string{"login_customer_id": "1234567890"},
		Status:               model.StatusActive,
	}
}

// TestConnectionUpdateIsBackedByACompareAndSwap is the live counterpart of
// campaign_repo_test.go's TestClaimVersionIsBackedByACompareAndSwap, and it exists
// because that test's own doc comment says what this file changes: "asserted against
// the SQL text because this package has no live-database harness in CI". It has one
// now, and the connection repo's version gate is the mechanism that keeps two
// concurrent bootstrap-system-account rotations from interleaving one run's account
// with another run's credential — the single write that the whole "one version-gated
// statement" design rests on.
//
// Source-text assertions cannot reach the property under test here. The gate is not
// "the string AND version = $n appears"; it is that a second writer holding the SAME
// expected version matches ZERO rows after the first commits, and that the repository
// tells that apart from a missing row. Only a real UPDATE can answer either.
func TestConnectionUpdateIsBackedByACompareAndSwap(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)

	projectID := dbtest.UniqueID(t, "conn")
	created, err := repo.Create(ctx, newGoogleAdsConn(projectID, "111"))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if created.Version == 0 {
		t.Fatalf("created connection version = 0, want a nonzero starting version")
	}
	stale := created.Version

	// First writer at the claimed version wins and BUMPS it. The bump is half the
	// mechanism: without it the loser below would also satisfy the predicate.
	first := newGoogleAdsConn(projectID, "222")
	updated, err := repo.Update(ctx, first, stale)
	if err != nil {
		t.Fatalf("first update at version %d: %v", stale, err)
	}
	if updated.Version != stale+1 {
		t.Errorf("version after update = %d, want %d — a write that does not bump the version "+
			"leaves the losing writer's precondition satisfied, and both rotations persist",
			updated.Version, stale+1)
	}
	if updated.AccountID != "222" {
		t.Errorf("account_id after update = %q, want %q", updated.AccountID, "222")
	}

	// Second writer still holding the pre-bump version. This is the failover case the
	// gate exists for: its advisory lock may well have been released server-side while
	// it was inside its upstream call, so nothing but this predicate stops it.
	second := newGoogleAdsConn(projectID, "333")
	if _, err := repo.Update(ctx, second, stale); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale-version update error = %v, want domain.ErrPreconditionFailed — a stale writer "+
			"that succeeds overwrites the winner's account with its own, which is precisely the "+
			"account/credential interleaving the single version-gated statement prevents", err)
	}

	// And the losing write left NOTHING behind. A gate that rejects the caller after
	// having already written some of the columns would be worse than no gate.
	after, err := repo.Get(ctx, projectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("get after rejected update: %v", err)
	}
	if after.AccountID != "222" || after.Version != stale+1 {
		t.Errorf("row after rejected update = (account %q, version %d), want (%q, %d): "+
			"the rejected write must not be partially applied",
			after.AccountID, after.Version, "222", stale+1)
	}
}

// TestConnectionUpdateTellsMissingApartFromStale pins the OTHER half of the no-row
// branch. Both outcomes produce zero rows from one UPDATE, and they call for opposite
// things from the caller: ErrPreconditionFailed means "re-read and retry with a fresh
// ETag", ErrNotFound means "there is nothing here to retry against". Collapsing them —
// the natural shape if the branch ever gets simplified — sends a caller into a retry
// loop against a row that does not exist.
func TestConnectionUpdateTellsMissingApartFromStale(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)

	projectID := dbtest.UniqueID(t, "conn")
	if _, err := repo.Update(ctx, newGoogleAdsConn(projectID, "111"), 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update against an absent row = %v, want domain.ErrNotFound", err)
	}

	created, err := repo.Create(ctx, newGoogleAdsConn(projectID, "111"))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	// A soft-deleted row is missing, not stale: the UPDATE's status <> 'deleted' term
	// excludes it, and so does the Get that classifies the miss. The version is CURRENT
	// here, so a classifier that consulted only the version would answer
	// ErrPreconditionFailed about a connection the project deliberately removed.
	if err := repo.Delete(ctx, projectID, model.ProviderGoogleAds, nil); err != nil {
		t.Fatalf("delete connection: %v", err)
	}
	if _, err := repo.Update(ctx, newGoogleAdsConn(projectID, "222"), created.Version); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update against a soft-deleted row at its current version = %v, want domain.ErrNotFound", err)
	}
}

// TestConnectionUpdateWithCredentialWritesBothInOneStatement pins what
// UpdateWithCredential is FOR. Writing the account and the credential as two statements
// leaves a window in which the row holds one rotation's account beside another's
// credential — a state that authenticates against the wrong account, which is the worst
// available outcome because it is the one that does not fail. Under the single gated
// statement the loser writes neither.
func TestConnectionUpdateWithCredentialWritesBothInOneStatement(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)

	projectID := dbtest.UniqueID(t, "conn")
	created, err := repo.Create(ctx, newGoogleAdsConn(projectID, "111"))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	rotated := newGoogleAdsConn(projectID, "222")
	if _, err := repo.UpdateWithCredential(ctx, rotated, []byte("ciphertext-v2"), created.Version); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// The losing rotation carries a DIFFERENT account and a DIFFERENT credential. If it
	// landed, the row would be some mixture of the two runs.
	loser := newGoogleAdsConn(projectID, "333")
	if _, err := repo.UpdateWithCredential(ctx, loser, []byte("ciphertext-v3"), created.Version); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale rotation error = %v, want domain.ErrPreconditionFailed", err)
	}

	after, err := repo.Get(ctx, projectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("get after rejected rotation: %v", err)
	}
	if after.AccountID != "222" {
		t.Errorf("account_id = %q, want 222", after.AccountID)
	}
	// Read the blob directly: the repository deliberately does not expose it on the
	// read model, and the pairing is the whole point of the assertion.
	var blob []byte
	if err := pool.QueryRow(ctx,
		`SELECT credentials FROM google_ads_connections WHERE project_id = $1 AND status <> 'deleted'`,
		projectID).Scan(&blob); err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if string(blob) != "ciphertext-v2" {
		t.Errorf("credentials = %q, want %q — the stored account and credential must come from "+
			"the SAME rotation", blob, "ciphertext-v2")
	}
}

// TestSoftDeletedConnectionIsIndistinguishableFromGetsNotFound pins the fact the
// system-account fallback had to be built around: after a DELETE, `Get` cannot tell an
// operator's deliberate disconnect apart from a project that never connected at all.
//
// Delete soft-deletes (status = 'deleted') and Get filters those rows out, so both states
// arrive at credsSource.resolve as the same domain.ErrNotFound. That sentinel is the fallback's
// trigger — and only that sentinel; every other failure means the project HAS a connection
// needing attention. So a delete used to return the project to the never-connected state and
// silently move its spend onto the Linux Foundation's ad account, which is not what an owner
// who removed their credentials asked for.
//
// It is pinned HERE, against a real delete, because the ambiguity is a property of the SQL: an
// in-memory fake returns ErrNotFound by construction and would pass against a Get that had lost
// its filter entirely. The resolution is NOT to make Get behave differently — a deleted
// connection genuinely is not a usable connection — but to ask a second, explicit question, which
// TestDisconnectedTellsADeliberateDisconnectApartFromNeverConnected below covers.
func TestSoftDeletedConnectionIsIndistinguishableFromGetsNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)

	projectID := dbtest.UniqueID(t, "project")
	if _, err := repo.Create(ctx, newGoogleAdsConn(projectID, "8666746580")); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.Get(ctx, projectID, model.ProviderGoogleAds); err != nil {
		t.Fatalf("get before delete: %v", err)
	}

	if err := repo.Delete(ctx, projectID, model.ProviderGoogleAds, nil); err != nil {
		t.Fatalf("delete connection: %v", err)
	}

	_, err := repo.Get(ctx, projectID, model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get after soft delete = %v, want domain.ErrNotFound — the credential resolver "+
			"falls back to the LF system account on ErrNotFound and ONLY on ErrNotFound, so any "+
			"other error here silently changes which account a disconnected project spends on", err)
	}

	// And the row is still there: a soft delete retains it for audit and undelete. If this
	// ever reads 0 the delete became a hard one, and the "soft" in every comment above is a lie.
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM google_ads_connections WHERE project_id = $1 AND status = 'deleted'`,
		projectID).Scan(&remaining); err != nil {
		t.Fatalf("count deleted rows: %v", err)
	}
	if remaining != 1 {
		t.Errorf("deleted rows for %s = %d, want 1 — the delete must be soft", projectID, remaining)
	}
}

// TestDisconnectedTellsADeliberateDisconnectApartFromNeverConnected binds the query the
// system-account fallback consults before it hands a project the LF-owned credential.
//
// The distinction Get cannot make (see the test above) is the whole point: "never connected" may
// fall back, "connected and then deliberately disconnected" must not. An owner who removes their
// ad account has said no, and reading that no as silence moves their spend onto the Linux
// Foundation's account without anyone deciding to. Disconnected answers it from the tombstone the
// soft delete leaves behind.
//
// This has to be a live test. The dispatch-level tests bind the BEHAVIOUR against a fake reader,
// so they stay green no matter what the SQL says — including if the predicate lost its
// status = 'deleted' clause and started reporting every connected project as disconnected, which
// would refuse the fallback to exactly the projects entitled to it. Only a real row can tell.
func TestDisconnectedTellsADeliberateDisconnectApartFromNeverConnected(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)

	never := dbtest.UniqueID(t, "never")
	disconnected := dbtest.UniqueID(t, "disconnected")
	connected := dbtest.UniqueID(t, "connected")

	for _, p := range []string{disconnected, connected} {
		if _, err := repo.Create(ctx, newGoogleAdsConn(p, "8666746580")); err != nil {
			t.Fatalf("create connection for %s: %v", p, err)
		}
	}
	if err := repo.Delete(ctx, disconnected, model.ProviderGoogleAds, nil); err != nil {
		t.Fatalf("delete connection: %v", err)
	}

	for name, tc := range map[string]struct {
		projectID string
		want      bool
		because   string
	}{
		"a project that never connected": {
			projectID: never, want: false,
			because: "it has said nothing, so the LF system account is the intended fallback",
		},
		"a project that disconnected": {
			projectID: disconnected, want: true,
			because: "the tombstone is an explicit no, and the fallback must withhold the LF account",
		},
		"a project with a live connection": {
			projectID: connected, want: false,
			because: "it never removed anything; reporting it disconnected would refuse a fallback " +
				"to a project whose own credential resolves fine",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := repo.Disconnected(ctx, tc.projectID, model.ProviderGoogleAds)
			if err != nil {
				t.Fatalf("Disconnected(%s): %v", tc.projectID, err)
			}
			if got != tc.want {
				t.Errorf("Disconnected(%s) = %v, want %v — %s", tc.projectID, got, tc.want, tc.because)
			}
		})
	}

	// A provider this repository does not know must ERROR rather than answer false. False is the
	// value that licenses the fallback, so a typo'd provider would quietly hand out the LF
	// credential; and the provider name reaches Table(), which composes the SQL.
	if _, err := repo.Disconnected(ctx, disconnected, model.Provider("google-adz")); err == nil {
		t.Error("Disconnected with an unknown provider returned no error; false is the value that " +
			"licenses the system-account fallback, so an unrecognised provider must never reach it")
	}
}

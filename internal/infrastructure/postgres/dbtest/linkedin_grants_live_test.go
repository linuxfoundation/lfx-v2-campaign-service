// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

func newLinkedInConn(projectID string) *model.Connection {
	return &model.Connection{
		ProjectID:            projectID,
		Provider:             model.ProviderLinkedInAds,
		Label:                "live-test",
		AccountID:            "538170226",
		EncryptedCredentials: []byte("ciphertext-v1"),
		ProviderConfig:       map[string]string{"org_id": "208777"},
		Status:               model.StatusActive,
	}
}

// TestLinkedInGrantsReplaceIsVersionGatedAndReadBack pins migration 000058 and the repository
// together (#581): a replace swaps the WHOLE set, bumps the row's version (the bump is what
// invalidates a cached credential/client for the old set), a writer holding the old version
// loses with ErrPreconditionFailed and writes nothing, and Get returns the set in account order.
func TestLinkedInGrantsReplaceIsVersionGatedAndReadBack(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)
	projectID := dbtest.UniqueID(t, "li-grants")
	created, err := repo.Create(ctx, newLinkedInConn(projectID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, gerr := repo.Get(ctx, projectID, model.ProviderLinkedInAds); gerr != nil || len(got.LinkedInGrants) != 0 {
		t.Fatalf("fresh row grants = %v, %v; want none", got, gerr)
	}

	first := []model.LinkedInAccountGrant{
		{AccountID: "509430019", OrgID: "208777", Label: "LF Events"},
		{AccountID: "500928401", OrgID: "12893459"},
	}
	upd, err := repo.ReplaceLinkedInAccountGrants(ctx, projectID, first, &model.Actor{Name: "t"}, created.Version)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if upd.Version != created.Version+1 {
		t.Fatalf("version after replace = %d, want %d", upd.Version, created.Version+1)
	}
	got, err := repo.Get(ctx, projectID, model.ProviderLinkedInAds)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := []model.LinkedInAccountGrant{
		{AccountID: "500928401", OrgID: "12893459"},
		{AccountID: "509430019", OrgID: "208777", Label: "LF Events"},
	}
	if !reflect.DeepEqual(got.LinkedInGrants, want) {
		t.Fatalf("grants read back = %+v, want %+v", got.LinkedInGrants, want)
	}

	// The stale writer loses and changes nothing.
	if _, err := repo.ReplaceLinkedInAccountGrants(ctx, projectID, nil, nil, created.Version); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale replace err = %v, want ErrPreconditionFailed", err)
	}
	if got, _ := repo.Get(ctx, projectID, model.ProviderLinkedInAds); len(got.LinkedInGrants) != 2 {
		t.Fatalf("stale replace changed the set: %+v", got.LinkedInGrants)
	}

	// An empty replace removes every grant.
	if _, err := repo.ReplaceLinkedInAccountGrants(ctx, projectID, nil, nil, upd.Version); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := repo.Get(ctx, projectID, model.ProviderLinkedInAds); len(got.LinkedInGrants) != 0 {
		t.Fatalf("cleared set = %+v, want none", got.LinkedInGrants)
	}

	// The table's own CHECK refuses a non-numeric id even past the installer's validation, and
	// the failed transaction leaves the version where it was.
	cur, _ := repo.Get(ctx, projectID, model.ProviderLinkedInAds)
	if _, err := repo.ReplaceLinkedInAccountGrants(ctx, projectID, []model.LinkedInAccountGrant{{AccountID: "urn:li:x", OrgID: "1"}}, nil, cur.Version); err == nil {
		t.Fatal("non-numeric account id was stored")
	}
	if after, _ := repo.Get(ctx, projectID, model.ProviderLinkedInAds); after.Version != cur.Version {
		t.Fatalf("a refused replace bumped the version %d -> %d", cur.Version, after.Version)
	}

	if _, err := repo.ReplaceLinkedInAccountGrants(ctx, dbtest.UniqueID(t, "absent"), nil, nil, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("replace on a missing row err = %v, want ErrNotFound", err)
	}
}

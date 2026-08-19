// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// credentialKey is a fixed 32-byte AES-256 key. It is a literal rather than a random
// value so a failure is reproducible: with a random key, a round-trip that broke only
// for certain key bytes would fail intermittently and be blamed on the database.
//
// A test key in the source is not a leaked secret. The real key arrives from a
// Kubernetes secret via the environment (see crypto.NewAESGCMFromBase64) and never
// appears here; this one opens nothing but the row this test just wrote.
var credentialKey = []byte("0123456789abcdef0123456789abcdef")

// newAESGCM builds the real encryptor. Every test in this file uses the PRODUCTION
// implementation, not a fake: a fake encryptor tested against a fake column proves
// only that two test doubles agree with each other.
func newAESGCM(t *testing.T) *crypto.AESGCM {
	t.Helper()
	enc, err := crypto.NewAESGCM(credentialKey)
	if err != nil {
		t.Fatalf("crypto.NewAESGCM: %v", err)
	}
	return enc
}

// TestLiveCredentialSurvivesTheRealByteaColumn is the property the credential path
// rests on and the one no existing test reaches: that AES-256-GCM output written to
// the real `credentials` BYTEA column comes back byte-identical and decrypts to the
// plaintext it went in as.
//
// Every other credential test in this package writes a literal like []byte("ciphertext-v1"),
// which is valid ASCII and therefore survives anything — including a column typed as
// text, a driver that re-encodes on the wire, or an encryptor that never ran. GCM output
// is none of those things: it is a random nonce followed by ciphertext and a tag, so it
// contains NUL bytes, invalid UTF-8, and every byte value. That is exactly the input that
// distinguishes a column which stores bytes from one that stores a string, and it is why
// the plaintext here is deliberately non-ASCII too.
//
// The assertions are in three parts, and dropping any one of them makes the test
// vacuous:
//
//  1. The decrypted plaintext EQUALS the original bytes. Alone this passes for an
//     encryptor that stores cleartext, since cleartext decrypts to itself.
//  2. The STORED COLUMN is not the plaintext. This is the negative assertion, and it
//     is the one that fails a passthrough encryptor. Without it the test cannot tell
//     encryption from a no-op.
//  3. The stored column is byte-identical to what Encrypt produced. This is what makes
//     the round-trip a statement about the DATABASE rather than about crypto: if the
//     column mangled a single byte, GCM authentication would fail, and the failure
//     would arrive as ErrDecryptionFailed — an error this test would otherwise report
//     as "decryption is broken" when the defect is in storage.
func TestLiveCredentialSurvivesTheRealByteaColumn(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)
	enc := newAESGCM(t)

	// Non-ASCII on purpose: a refresh token is base64 in practice, but a plaintext
	// that is already safe ASCII cannot detect a column that silently transcodes.
	plaintext := []byte("refresh-token\x00\xff\xfe-Ω-secret")

	sealed, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(sealed, plaintext) {
		// Guards the guard: if Encrypt is a passthrough, every assertion below still
		// passes on a working database, so the round-trip would certify a no-op.
		t.Fatal("Encrypt returned its input unchanged; the encryptor is a passthrough " +
			"and the round-trip below would prove nothing")
	}

	projectID := dbtest.UniqueID(t, "cred")
	conn := newGoogleAdsConn(projectID, "222")
	conn.EncryptedCredentials = sealed
	created, err := repo.Create(ctx, conn)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Read through the REPOSITORY, not a hand-written SELECT: the scan path is part of
	// what is under test. A repo that dropped the credentials column from its select
	// list would pass a test that queried the column directly.
	got, err := repo.Get(ctx, projectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// (3) The bytes survived storage intact.
	if !bytes.Equal(got.EncryptedCredentials, sealed) {
		t.Fatalf("credentials column did not round-trip: stored %d bytes, read back %d; "+
			"GCM authentication cannot survive this, so a decrypt failure here would be "+
			"reported against the encryptor when the defect is in the column",
			len(sealed), len(got.EncryptedCredentials))
	}

	// (2) The NEGATIVE assertion. A broken encryptor that stored cleartext would
	// satisfy (1) and (3); only this fails it.
	if bytes.Equal(got.EncryptedCredentials, plaintext) {
		t.Fatal("the credentials column holds the PLAINTEXT: the blob is being stored " +
			"unencrypted, and a decrypt-equals-plaintext assertion cannot detect it")
	}
	if bytes.Contains(got.EncryptedCredentials, []byte("secret")) {
		t.Fatalf("the credentials column contains a recognisable fragment of the "+
			"plaintext (%q); the stored blob is not sealed", "secret")
	}

	// (1) The round-trip closes.
	opened, err := enc.Decrypt(got.EncryptedCredentials)
	if err != nil {
		t.Fatalf("Decrypt of the stored blob: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		// Report the mismatch by SHAPE, never by value: this failure prints into CI job
		// output, and the same lines get copied into tests that do hold a real blob. Length
		// separates "truncated/padded" from "same size, different bytes"; the digests
		// separate the latter from a fault this test cannot see.
		t.Fatalf("decrypted blob does not match the plaintext it was sealed from: "+
			"got %d bytes sha256=%x, want %d bytes sha256=%x",
			len(opened), sha256.Sum256(opened), len(plaintext), sha256.Sum256(plaintext))
	}
	if created.Version != got.Version {
		t.Fatalf("Create returned version %d but Get reads %d", created.Version, got.Version)
	}
}

// TestLiveSetCredentialRotatesTheBlobAndBumpsVersion covers SetCredential, which had no
// live coverage at all: the connection tests exercise Create, Update, UpdateWithCredential
// and Delete, and SetCredential is the one write the credential-rotation path actually
// calls.
//
// It is also the only connection write that is NOT version-gated, which is why the
// version bump is asserted rather than assumed. The handler publishes the returned row's
// version as the caller's ETag; if the UPDATE did not bump it, the next If-Match would
// succeed against a version whose row no longer exists in that form, and every assertion
// about the credential itself would still pass.
func TestLiveSetCredentialRotatesTheBlobAndBumpsVersion(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)
	enc := newAESGCM(t)

	first, err := enc.Encrypt([]byte("token-before-rotation"))
	if err != nil {
		t.Fatalf("Encrypt first: %v", err)
	}
	projectID := dbtest.UniqueID(t, "setcred")
	conn := newGoogleAdsConn(projectID, "333")
	conn.EncryptedCredentials = first
	created, err := repo.Create(ctx, conn)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rotatedPlaintext := []byte("token-after-rotation")
	second, err := enc.Encrypt(rotatedPlaintext)
	if err != nil {
		t.Fatalf("Encrypt second: %v", err)
	}
	// Two seals of two different plaintexts must differ; if they did not, the
	// "the blob changed" assertion below would be untestable.
	if bytes.Equal(first, second) {
		t.Fatal("two Encrypt calls produced identical blobs")
	}

	actor := &model.Actor{Username: "rotation-bot"}
	updated, err := repo.SetCredential(ctx, projectID, model.ProviderGoogleAds, second, actor)
	if err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	if updated.Version <= created.Version {
		t.Fatalf("SetCredential returned version %d, want greater than the created %d; "+
			"the row it hands back becomes the caller's ETag, so an unbumped version "+
			"lets the NEXT If-Match succeed against a state this write replaced",
			updated.Version, created.Version)
	}

	// The rotation is visible to a fresh read, and it is the NEW blob.
	got, err := repo.Get(ctx, projectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("Get after rotation: %v", err)
	}
	if !bytes.Equal(got.EncryptedCredentials, second) {
		t.Fatal("the stored credential is not the rotated blob")
	}
	if bytes.Equal(got.EncryptedCredentials, first) {
		t.Fatal("SetCredential left the PREVIOUS credential in place; the rotation did " +
			"not reach the column")
	}
	if got.Version != updated.Version {
		t.Fatalf("Get reads version %d but SetCredential returned %d", got.Version, updated.Version)
	}

	// The rotated blob still opens to its plaintext, and is not that plaintext.
	if bytes.Equal(got.EncryptedCredentials, rotatedPlaintext) {
		t.Fatal("the rotated credential is stored as plaintext")
	}
	opened, err := enc.Decrypt(got.EncryptedCredentials)
	if err != nil {
		t.Fatalf("Decrypt rotated blob: %v", err)
	}
	if !bytes.Equal(opened, rotatedPlaintext) {
		// Shape, not value — see the equivalent assertion in
		// TestLiveCredentialSurvivesTheRealByteaColumn.
		t.Fatalf("the rotated blob does not open to the rotated plaintext: "+
			"got %d bytes sha256=%x, want %d bytes sha256=%x",
			len(opened), sha256.Sum256(opened), len(rotatedPlaintext), sha256.Sum256(rotatedPlaintext))
	}

	// SetCredential touches ONLY the credential. The account and config columns are
	// what an interleaved write would corrupt, and nothing else asserts they survive.
	if got.AccountID != created.AccountID {
		t.Fatalf("SetCredential changed account_id from %q to %q", created.AccountID, got.AccountID)
	}
	if got.ProviderConfig["login_customer_id"] != created.ProviderConfig["login_customer_id"] {
		t.Fatalf("SetCredential changed login_customer_id from %q to %q",
			created.ProviderConfig["login_customer_id"], got.ProviderConfig["login_customer_id"])
	}
}

// TestLiveSetCredentialOnAMissingConnectionIsNotFound pins the arm that separates
// "there is no such connection" from "the write silently matched nothing".
//
// SetCredential's WHERE has no version predicate, so pgx.ErrNoRows here can only mean
// the row is absent or soft-deleted. Returning anything but ErrNotFound would let a
// rotation against a deleted connection report success while writing nowhere.
func TestLiveSetCredentialOnAMissingConnectionIsNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)
	enc := newAESGCM(t)

	sealed, err := enc.Encrypt([]byte("token-for-nobody"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	actor := &model.Actor{Username: "rotation-bot"}

	// A project that was never created.
	absent := dbtest.UniqueID(t, "setcred-absent")
	if _, err := repo.SetCredential(ctx, absent, model.ProviderGoogleAds, sealed, actor); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetCredential on an absent connection = %v, want domain.ErrNotFound", err)
	}

	// A project that was created and then soft-deleted. This is the case the
	// `status <> 'deleted'` predicate exists for, and the one an absent-row test
	// cannot reach: the row is still physically present.
	projectID := dbtest.UniqueID(t, "setcred-deleted")
	if _, err := repo.Create(ctx, newGoogleAdsConn(projectID, "444")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(ctx, projectID, model.ProviderGoogleAds, actor); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.SetCredential(ctx, projectID, model.ProviderGoogleAds, sealed, actor); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetCredential on a soft-deleted connection = %v, want domain.ErrNotFound; "+
			"the row is still present, so only the status predicate can exclude it", err)
	}
}

// TestLiveConnectionCreateConflictsOnTheSingletonIndex covers the Create -> ErrConflict
// arm. The mapping from a raw 23505 to domain.ErrConflict (which the handler answers as
// 409) is asserted nowhere against a live database for this repo, and it cannot be: the
// constraint that fires is a PARTIAL unique index on project_id WHERE status <> 'deleted',
// so whether a second Create conflicts depends on the index predicate, not on the SQL text.
//
// The soft-delete half is the reason the index is partial at all. If a deleted row still
// occupied the slot, a project could never reconnect after disconnecting — and no
// source-text assertion distinguishes that from the working behaviour.
func TestLiveConnectionCreateConflictsOnTheSingletonIndex(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)

	projectID := dbtest.UniqueID(t, "singleton")
	if _, err := repo.Create(ctx, newGoogleAdsConn(projectID, "555")); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// A second live connection for the same project on the same provider is refused,
	// and refused AS a conflict rather than as an opaque database error.
	_, err := repo.Create(ctx, newGoogleAdsConn(projectID, "666"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Create = %v, want domain.ErrConflict; the partial unique index on "+
			"project_id WHERE status <> 'deleted' is what makes the connection a singleton, "+
			"and a raw 23505 reaching the handler answers 500 instead of 409", err)
	}

	// After a soft delete the slot is free again. Without this the singleton index
	// would be a one-way door.
	if err := repo.Delete(ctx, projectID, model.ProviderGoogleAds, &model.Actor{Username: "live-test"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	recreated, err := repo.Create(ctx, newGoogleAdsConn(projectID, "777"))
	if err != nil {
		t.Fatalf("Create after soft delete = %v, want success; the unique index predicate "+
			"must exclude deleted rows or a project can never reconnect", err)
	}
	if recreated.AccountID != "777" {
		t.Fatalf("recreated connection has account %q, want the new one", recreated.AccountID)
	}
}

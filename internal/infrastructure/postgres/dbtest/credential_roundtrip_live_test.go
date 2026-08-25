// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"testing"
	"unicode/utf8"

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

// gcmSizes returns the nonce and tag lengths of a SEPARATE cipher.AEAD built the way
// crypto.NewAESGCM builds its own today: AES with credentialKey, wrapped in cipher.NewGCM.
//
// Be precise about what this does and does not buy, because the distinction decides what a
// failure here means. It reads the sizes off a real AEAD rather than writing 12 and 16 as
// literals, which keeps the two numbers consistent with each other and self-describing. It
// does NOT read the AEAD that crypto.AESGCM actually uses — there is no accessor for it —
// so it is still PINNING cipher.NewGCM's defaults, just constructing them instead of
// spelling them out.
//
// The consequence: if production moved to cipher.NewGCMWithNonceSize, this helper would go
// on reporting 12 and the size assertions would fail against a correct blob. That is the
// same failure a hard-coded 12 would produce, and this construction does not prevent it.
// What it does prevent is the two numbers drifting apart from each other or from the cipher
// they describe.
//
// Exporting NonceSize/TagSize from the crypto package would close the gap properly, and is
// deliberately not done: it widens the production API for a test's benefit, which this
// ticket's knowledge log already records as the wrong instinct. The gap is accepted and
// named here rather than papered over.
func gcmSizes(t *testing.T) (nonceSize, tagSize int) {
	t.Helper()
	block, err := aes.NewCipher(credentialKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	return aead.NonceSize(), aead.Overhead()
}

// textHostileSeals is how many Encrypt attempts sealTextHostile makes before giving up.
//
// The bound is what keeps a precondition from becoming a hang. A single seal is already
// overwhelmingly likely to qualify — measured over 200,000 seals of this file's plaintext,
// 0 were text-safe and 100% were invalid UTF-8, while only 19.0% carried a NUL byte
// outright — so 64 attempts is not a budget this is expected to spend. It exists so that
// an Encrypt which somehow stopped producing binary output fails the test with a message,
// instead of spinning forever in CI.
//
// The 19% figure is why the disqualifying test below is an OR and not an AND. A sealed
// blob here is 54 bytes (12-byte nonce + 26 ciphertext + 16 tag), so P(no NUL) =
// (255/256)^54 ≈ 0.8095 and P(NUL) ≈ 0.1905 — the measurement and the arithmetic agree.
// Demanding a NUL *and* invalid UTF-8 would put the per-attempt hit rate at that 19%, and
// 64 independent misses at ≈ 1.4e-6 — rare, but a t.Fatalf on CORRECT code, which is a
// red build nothing can act on. Under the OR the per-attempt rate is the measured 100%.
const textHostileSeals = 64

// sealTextHostile returns Encrypt output that a TEXT column provably cannot store
// unchanged, and FAILS the test if it cannot obtain one.
//
// This exists because the premise of TestLiveCredentialSurvivesTheRealByteaColumn is not
// self-evidently true. That test's job is to catch the credentials column being typed TEXT
// instead of BYTEA, and it can only do that if the bytes it writes are bytes TEXT refuses.
// GCM output is a RANDOM nonce followed by ciphertext and a tag, so which byte values
// appear is drawn fresh on every run — "contains a NUL byte and invalid UTF-8" is a
// probabilistic property of the sample, not a guarantee of the algorithm. On a run whose
// ciphertext happened to be text-safe, a TEXT column would round-trip it unchanged and the
// test would pass while missing the exact regression it was written for, with nothing to
// signal the near miss: the test is green either way.
//
// So the property is ASSERTED rather than assumed. A silent retry or an unbounded loop
// would reintroduce the same class of defect in a new place — the first hides a
// no-longer-binary Encrypt, the second converts it into a hung job — so the search is
// bounded and its exhaustion is a Fatal.
//
// EITHER disqualifying condition suffices, so the test is an OR. The two are refused for
// the same reason and independently: on a UTF8 server PostgreSQL rejects a NUL byte in a
// text value ("invalid byte sequence for encoding UTF8: 0x00") and rejects invalid UTF-8
// the same way ("...: 0xff"). Verified against PG16 rather than assumed — a NUL is in fact
// a well-formed UTF-8 encoding of U+0000, so "contains a NUL" is not a special case of
// "invalid UTF-8" and the server's refusal of it is a separate rule.
//
// Requiring the PAIR was the earlier form and it was a defect: it dropped the per-attempt
// hit rate from 100% to 19%, which made exhausting all 64 attempts a ~1.4e-6 red build on
// a correct implementation. Since either condition alone already makes the blob
// unstorable as text, the conjunction bought no strength for that risk. See
// textHostileSeals.
func sealTextHostile(t *testing.T, enc *crypto.AESGCM, plaintext []byte) []byte {
	t.Helper()

	for range textHostileSeals {
		sealed, err := enc.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if bytes.IndexByte(sealed, 0) >= 0 || !utf8.Valid(sealed) {
			return sealed
		}
	}
	// Report by SHAPE, never by value — this function handles sealed credential bytes,
	// and the message renders into CI job output. See the equivalent note on the
	// decrypted-blob assertions below.
	t.Fatalf("no ciphertext in %d Encrypt calls contained either a NUL byte or invalid "+
		"UTF-8, so the round-trip below could not distinguish a BYTEA column from a TEXT "+
		"one; either Encrypt stopped producing binary output or it is no longer randomised",
		textHostileSeals)
	return nil
}

// TestLiveCredentialSurvivesTheRealByteaColumn is the property the credential path
// rests on and the one no existing test reaches: that AES-256-GCM output written to
// the real `credentials` BYTEA column comes back byte-identical and decrypts to the
// plaintext it went in as.
//
// Every other credential test in this package writes a literal like []byte("ciphertext-v1"),
// which is valid ASCII and therefore survives anything — including a column typed as
// text, a driver that re-encodes on the wire, or an encryptor that never ran. GCM output
// is none of those things: it is a random nonce followed by ciphertext and a tag, so its
// bytes are drawn from the full 0x00–0xff range rather than from printable ASCII. A single
// 54-byte sample obviously cannot contain every byte value, and it carries a NUL only
// ~19% of the time; what it is essentially always is invalid UTF-8 (measured: 200,000 of
// 200,000). That is exactly the input that distinguishes a column which stores bytes from
// one that stores a string, and it is why the plaintext here is deliberately non-ASCII too.
//
// "Ordinarily" is doing real work in that sentence, which is why the ciphertext comes from
// sealTextHostile rather than a bare Encrypt. The nonce is random, so whether a given
// sample is text-hostile is a property of THAT sample; a run that drew text-safe bytes
// would pass against a TEXT column and silently miss the regression. sealTextHostile
// asserts the precondition instead of trusting it.
//
// The assertions are in three parts. They are COMPLEMENTARY rather than each
// individually load-bearing, and an earlier version of this comment overstated that by
// saying dropping any one makes the test vacuous. It does not: the AES-GCM length
// identity below independently rejects a passthrough encryptor, since cleartext is 26
// bytes and a sealed blob is 54, so (2) is not the only thing standing between this test
// and a no-op. What each part adds:
//
//  1. The decrypted plaintext EQUALS the original bytes. Alone this passes for an
//     encryptor that stores cleartext, since cleartext decrypts to itself.
//  2. The STORED COLUMN is not the plaintext. The negative assertion: it fails a
//     passthrough encryptor directly, and does so on IDENTITY rather than on size, so
//     it still holds for a hypothetical cleartext encoding that happened to match the
//     sealed length. The length check below covers the same class from the other side.
//  3. The stored column is byte-identical to what Encrypt produced. This LOCALISES a
//     storage defect rather than being the only thing that detects one: if the column
//     mangled a single byte, GCM authentication would fail anyway and (1) would go red
//     — but as ErrDecryptionFailed, which reads as "decryption is broken" when the
//     defect is in storage. Asserting the bytes names the real culprit.
func TestLiveCredentialSurvivesTheRealByteaColumn(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := connectionRepo(pool)
	enc := newAESGCM(t)

	// Non-ASCII on purpose: a refresh token is base64 in practice, but a plaintext
	// that is already safe ASCII cannot detect a column that silently transcodes.
	plaintext := []byte("refresh-token\x00\xff\xfe-Ω-secret")

	// Not a bare Encrypt: the round-trip's whole premise is that these bytes are bytes a
	// TEXT column cannot hold. See sealTextHostile.
	sealed := sealTextHostile(t, enc, plaintext)
	if bytes.Equal(sealed, plaintext) {
		// An EARLY DIAGNOSTIC, not the guarantee. If Encrypt is a passthrough this names
		// it here, at the seal, instead of leaving it to be inferred from a failure after
		// a database round-trip. It is not what CATCHES a passthrough: the negative
		// column assertion below does that on its own, and the knowledge log records
		// exactly that — with this check disabled (`if false &&`), a passthrough
		// encryptor still fails on "the credentials column holds the PLAINTEXT".
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
		// Length AND digest. Lengths alone cannot describe the failure this assertion most
		// needs to report: a column that returns the right NUMBER of bytes with different
		// CONTENT — an encoding round-trip, a truncating-then-padding driver — prints two
		// identical numbers and reads like a passing run that somehow failed. The digests
		// differ whenever the bytes do, at any length, and neither reveals key material.
		t.Fatalf("credentials column did not round-trip: stored %d bytes sha256=%x, read "+
			"back %d bytes sha256=%x; GCM authentication cannot survive this, so a decrypt "+
			"failure here would be reported against the encryptor when the defect is in the column",
			len(sealed), sha256.Sum256(sealed),
			len(got.EncryptedCredentials), sha256.Sum256(got.EncryptedCredentials))
	}

	// (2) The NEGATIVE assertion. A broken encryptor that stored cleartext would
	// satisfy (1) and (3); only this fails it.
	if bytes.Equal(got.EncryptedCredentials, plaintext) {
		t.Fatal("the credentials column holds the PLAINTEXT: the blob is being stored " +
			"unencrypted, and a decrypt-equals-plaintext assertion cannot detect it")
	}
	// The stored blob has the SHAPE of AES-GCM output: a 12-byte nonce, the ciphertext
	// (which GCM, a stream mode, makes exactly as long as the plaintext), and a 16-byte
	// tag. This replaced a `bytes.Contains(blob, []byte("secret"))` search for a plaintext
	// fragment, which was the wrong TOOL for the job in both directions. GCM ciphertext is
	// pseudorandom, so it can legitimately contain the six bytes `secret` — for this
	// 54-byte blob that is at most (54-6+1)/256^6 = 1.74e-13, about 1 in 5.7 trillion —
	// which is a nonzero chance of failing correct encryption. That figure is a UNION
	// BOUND, not the exact probability: summing the 49 offsets double-counts blobs
	// carrying two disjoint occurrences (`secretsecret`), so the true value is slightly
	// lower. `secret` having no proper self-overlap rules out OVERLAPPING placements only.
	// The bound is what the argument needs — the risk is nonzero either way. And its absence
	// proved nothing either: an encryptor that stored ROT13 of the plaintext, or the
	// plaintext with one byte flipped, contains no literal `secret` and passes.
	//
	// That is precisely the class of probabilistic assertion `sealTextHostile` was added
	// to this file to eliminate, still live one function away. A length identity is the
	// deterministic replacement: it holds for EVERY sample rather than almost all of them,
	// and it is the invariant a passthrough actually violates — cleartext is 26 bytes, not
	// 54, so it fails here as well as on the equality check above. It also catches the
	// encoding wrappers a substring search cannot see: a hex- or base64-encoded blob has
	// the wrong length, and so does any encryptor that drops the nonce or the tag.
	//
	// The expected size comes from gcmSizes, which reads NonceSize()/Overhead() off a
	// cipher.AEAD built the same way crypto.AESGCM builds its own — not from a hard-coded
	// 54, and not from a new exported constant on the production package. A test does not
	// get to widen the production API. See gcmSizes for what that construction does and does
	// NOT guarantee: it keeps the nonce and tag sizes consistent with the cipher they
	// describe, but it still pins cipher.NewGCM's defaults rather than reading production's
	// own AEAD, which has no accessor.
	nonceSize, tagSize := gcmSizes(t)
	wantSealed := nonceSize + len(plaintext) + tagSize
	if len(got.EncryptedCredentials) != wantSealed {
		// Shape only: lengths, never bytes. See the note on the decrypt assertion below.
		t.Fatalf("the stored blob is %d bytes, want %d (a %d-byte nonce + %d-byte "+
			"ciphertext + %d-byte tag); it does not have the shape of AES-GCM output, so "+
			"it is not a sealed credential",
			len(got.EncryptedCredentials), wantSealed,
			nonceSize, len(plaintext), tagSize)
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
// Like createConn and deleteConn, it is not version-gated — updateConn is the only
// connection write that parses an If-Match — which is why the version bump is asserted
// rather than assumed. The bump is what INVALIDATES ETags issued before the rotation: a
// caller still holding the pre-rotation version must not be able to satisfy a later
// If-Match against a row this write has already replaced. (The set-credential handler
// discards the returned row and answers 204, so it does not itself publish the new
// version as an ETag; the row is still the repo's contract, and every assertion about the
// credential bytes below would pass whether or not the version moved.)
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
	// An EARLY DIAGNOSTIC, on the same footing as the passthrough check in
	// TestLiveCredentialSurvivesTheRealByteaColumn: it names a degenerate encryptor at
	// the seal rather than letting it surface as a confusing rotation failure. It is not
	// what proves the rotation reached the column — the `got != first` assertion below
	// does that on its own, and it fails whether or not the two seals are distinguishable
	// here.
	if bytes.Equal(first, second) {
		t.Fatal("two Encrypt calls produced identical blobs")
	}

	actor := &model.Actor{Username: "rotation-bot"}
	updated, err := repo.SetCredential(ctx, projectID, model.ProviderGoogleAds, second, actor)
	if err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	// The EXACT increment, not merely "greater": `<=` passes for a version+2 as readily
	// as version+1, and a double bump means a second write ran. connection_live_test.go
	// pins `!= stale+1` for the same reason.
	if updated.Version != created.Version+1 {
		t.Fatalf("SetCredential returned version %d, want exactly the created %d + 1; "+
			"the bump is what invalidates ETags issued before the rotation, so an "+
			"unbumped version lets a stale If-Match succeed against a state this write "+
			"replaced, and a double bump means a second write ran",
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

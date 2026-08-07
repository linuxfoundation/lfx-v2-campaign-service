// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func newTestKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestAESGCM_RoundTrip(t *testing.T) {
	enc, err := NewAESGCM(newTestKey(t))
	if err != nil {
		t.Fatalf("NewAESGCM: %v", err)
	}
	plaintext := []byte(`{"refresh_token":"secret","client_id":"abc"}`)

	ct, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ct, plaintext) {
		t.Fatal("ciphertext must not contain plaintext")
	}

	got, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestAESGCM_DistinctNoncePerMessage(t *testing.T) {
	enc, _ := NewAESGCM(newTestKey(t))
	a, _ := enc.Encrypt([]byte("same"))
	b, _ := enc.Encrypt([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("expected distinct ciphertexts for the same plaintext (random nonce)")
	}
}

func TestNewAESGCM_RejectsWrongKeySize(t *testing.T) {
	if _, err := NewAESGCM([]byte("too-short")); err != ErrKeySize {
		t.Fatalf("expected ErrKeySize, got %v", err)
	}
}

func TestNewAESGCMFromBase64(t *testing.T) {
	key := newTestKey(t)
	enc, err := NewAESGCMFromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewAESGCMFromBase64: %v", err)
	}
	if _, err := enc.Encrypt([]byte("x")); err != nil {
		t.Fatalf("Encrypt after base64 key: %v", err)
	}
}

func TestAESGCM_DecryptRejectsShortInput(t *testing.T) {
	enc, _ := NewAESGCM(newTestKey(t))
	if _, err := enc.Decrypt([]byte("short")); err != ErrCiphertextTooShort {
		t.Fatalf("expected ErrCiphertextTooShort, got %v", err)
	}
}

// The interesting range is the one BETWEEN the two minima: at least a full nonce, but
// short of nonce+tag. Such a value is provably truncated — Seal always appends
// Overhead() tag bytes, even to an empty plaintext — yet a `len(sealed) < NonceSize()`
// check alone lets it reach Open, where it fails authentication and is reported as the
// wrong-or-rotated-KEY condition (500, page ops, every connection presumed broken)
// instead of one malformed row (400).
//
// Deleting the `+overhead` term in Decrypt must fail this test. The exact boundary
// matters, so both ends are pinned: ns+overhead-1 is the last rejected length, and a
// genuine seal of empty plaintext — exactly ns+overhead bytes — must still OPEN.
func TestAESGCM_DecryptRejectsTruncatedBelowNonceAndTag(t *testing.T) {
	enc, err := NewAESGCM(newTestKey(t))
	if err != nil {
		t.Fatalf("NewAESGCM: %v", err)
	}
	sealed, err := enc.Encrypt(nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	minLen := len(sealed) // an empty message seals to exactly nonce+tag
	if _, err := enc.Decrypt(sealed); err != nil {
		t.Fatalf("a genuine seal of empty plaintext must decrypt, got %v", err)
	}

	// nonceSize is minLen-Overhead; walk the whole nonce..nonce+tag-1 window.
	nonceSize := minLen - 16 // AES-GCM tag is 16 bytes
	for n := nonceSize; n < minLen; n++ {
		_, derr := enc.Decrypt(sealed[:n])
		if !errors.Is(derr, ErrCiphertextTooShort) {
			t.Errorf("len=%d (nonce=%d, min=%d): got %v, want ErrCiphertextTooShort — "+
				"a value shorter than nonce+tag cannot be our own output, so it is a "+
				"malformed ROW, not an authentication failure",
				n, nonceSize, minLen, derr)
		}
		if errors.Is(derr, ErrDecryptionFailed) {
			t.Errorf("len=%d: truncated blob misclassified as an auth failure — this is "+
				"the path that maps to 500 and pages ops for a deployment-wide key problem",
				n)
		}
	}
}

func TestAESGCM_DecryptTamperedIsAuthFailure(t *testing.T) {
	enc, _ := NewAESGCM(newTestKey(t))
	ct, _ := enc.Encrypt([]byte("secret"))
	ct[len(ct)-1] ^= 0xFF // flip a byte in the ciphertext body
	_, err := enc.Decrypt(ct)
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed for tampered ciphertext, got %v", err)
	}
	// And it must NOT be classified as a format error.
	if errors.Is(err, ErrCiphertextTooShort) {
		t.Fatal("tampered ciphertext misclassified as a format error")
	}
}

func TestAESGCM_DecryptWrongKeyIsAuthFailure(t *testing.T) {
	enc1, _ := NewAESGCM(newTestKey(t))
	enc2, _ := NewAESGCM(newTestKey(t))
	ct, _ := enc1.Encrypt([]byte("secret"))
	if _, err := enc2.Decrypt(ct); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed for wrong key, got %v", err)
	}
}

// TestAESGCM_DecryptErrorsCarryDomainClassification pins the half of the contract that
// leaves this package. Nothing above internal/infrastructure imports crypto — the dispatch
// and service layers depend on domain.Encryptor — so the sentinels above are invisible to
// every caller that has to decide between "this connection row is bad" (400) and "the
// deployment's key is wrong" (500 + page ops). The wrapped domain sentinel is the ONLY
// thing that crosses, and dropping the wrap would leave every test above green while the
// dispatch layer silently fell into its unclassified default.
func TestAESGCM_DecryptErrorsCarryDomainClassification(t *testing.T) {
	enc, _ := NewAESGCM(newTestKey(t))

	_, short := enc.Decrypt([]byte("short"))
	if !errors.Is(short, domain.ErrCredentialsMalformed) {
		t.Errorf("short ciphertext: err = %v, want errors.Is(err, domain.ErrCredentialsMalformed)", short)
	}
	if errors.Is(short, domain.ErrCredentialDecryptionFailed) {
		t.Errorf("short ciphertext: err = %v, must not read as an authentication failure — "+
			"nothing was ever authenticated, and this must not page ops", short)
	}

	other, _ := NewAESGCM(newTestKey(t))
	ct, _ := enc.Encrypt([]byte("secret"))
	_, wrongKey := other.Decrypt(ct)
	if !errors.Is(wrongKey, domain.ErrCredentialDecryptionFailed) {
		t.Errorf("wrong key: err = %v, want errors.Is(err, domain.ErrCredentialDecryptionFailed)", wrongKey)
	}
	if errors.Is(wrongKey, domain.ErrCredentialsMalformed) {
		t.Errorf("wrong key: err = %v, must not read as bad row data — the blob is well formed "+
			"and it is the application key that is wrong", wrongKey)
	}
}

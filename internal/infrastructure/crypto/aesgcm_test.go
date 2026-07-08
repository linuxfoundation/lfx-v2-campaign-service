// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
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

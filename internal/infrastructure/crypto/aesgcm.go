// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package crypto provides application-level AES-256-GCM encryption for
// credential blobs. The key is supplied from a Kubernetes secret via the
// environment and lives only in the application — never in the database.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// KeySize is the required AES-256 key length in bytes.
const KeySize = 32

// ErrKeySize is returned when the configured key is not 32 bytes.
var ErrKeySize = fmt.Errorf("encryption key must be %d bytes (AES-256)", KeySize)

// ErrCiphertextTooShort is returned when a ciphertext is too short to contain a nonce.
var ErrCiphertextTooShort = errors.New("ciphertext too short")

// AESGCM implements domain.Encryptor using AES-256-GCM. The nonce is randomly
// generated per message and prepended to the ciphertext.
type AESGCM struct {
	aead cipher.AEAD
}

// NewAESGCM constructs an AESGCM from a 32-byte key.
func NewAESGCM(key []byte) (*AESGCM, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	return &AESGCM{aead: aead}, nil
}

// NewAESGCMFromBase64 constructs an AESGCM from a base64-encoded 32-byte key
// (the form supplied via the k8s secret / env var).
func NewAESGCMFromBase64(encoded string) (*AESGCM, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	return NewAESGCM(key)
}

// Encrypt seals plaintext, returning nonce||ciphertext.
func (a *AESGCM) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	// Seal appends the ciphertext to nonce, so the nonce prefixes the result.
	return a.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens nonce||ciphertext produced by Encrypt.
func (a *AESGCM) Decrypt(sealed []byte) ([]byte, error) {
	ns := a.aead.NonceSize()
	if len(sealed) < ns {
		return nil, ErrCiphertextTooShort
	}
	nonce, ciphertext := sealed[:ns], sealed[ns:]
	plaintext, err := a.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return plaintext, nil
}

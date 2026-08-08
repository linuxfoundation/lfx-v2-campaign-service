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
	"fmt"
	"io"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// KeySize is the required AES-256 key length in bytes.
const KeySize = 32

// ErrKeySize is returned when the configured key is not 32 bytes.
var ErrKeySize = fmt.Errorf("encryption key must be %d bytes (AES-256)", KeySize)

// The two Decrypt failures below carry DIFFERENT domain classifications, and the
// wrapping is the mechanism that carries them: callers live above this package (the
// dispatch and service layers depend on domain.Encryptor, not on crypto), so they
// cannot import these sentinels to tell the cases apart. Wrapping the domain
// sentinel lets `errors.Is` do it without inverting the dependency. Both remain
// usable as `crypto.Err…` inside this package and its tests, and ErrCiphertextTooShort
// is still returned by identity so an `==` comparison keeps working.

// ErrCiphertextTooShort is returned when a sealed value is too short to be GCM
// output at all — a malformed-input / data error. The blob is never authenticated,
// so this proves the stored ROW is bad and nothing else: domain.ErrCredentialsMalformed,
// which credsSource.resolve wraps with domain.ErrConnectionNotUsable, mapping to
// **400** (not 422 — an earlier revision of this comment said 422 and was wrong about
// the contract it was describing; the resolve path is the authority, see
// internal/dispatch/creds.go).
var ErrCiphertextTooShort = fmt.Errorf("%w: ciphertext too short", domain.ErrCredentialsMalformed)

// ErrDecryptionFailed is returned when GCM authentication fails — tampered data
// or a wrong/rotated key. This is an infrastructure/security condition (map to
// 500 and alert ops), distinct from a malformed-input error: a wrong deployment key
// fails EVERY connection, so it must not be reported as a per-connection data
// problem. Hence domain.ErrCredentialDecryptionFailed, not ErrCredentialsMalformed.
var ErrDecryptionFailed = fmt.Errorf("%w: decryption authentication failed", domain.ErrCredentialDecryptionFailed)

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
	// The minimum is nonce + AUTHENTICATION TAG, not nonce alone. Seal appends a
	// tag of Overhead() bytes to every message, including an empty one, so any
	// value shorter than ns+Overhead() cannot be output this package produced —
	// it is provably truncated, and no key could ever open it.
	//
	// Checking only `< ns` would let that range fall through to Open, which fails
	// authentication and is therefore classified as ErrDecryptionFailed — the
	// condition that maps to 500 and pages ops. That classification is deliberately
	// conservative rather than a diagnosis: an authentication failure is equally
	// consistent with a wrong or rotated deployment key (every connection broken)
	// and with corruption of this ONE full-length row (every other connection
	// fine), and GCM cannot tell them apart, so neither this package nor the
	// handler claims to. A provably truncated blob is different in kind — it is
	// decidable here, and it is always about one row — so letting it reach Open
	// would convert a certainty into that ambiguity and put a single bad row into
	// the arm that pages someone. Rejecting the full minimum keeps the blast radius
	// honest: what is knowably one bad row is reported as one bad row.
	ns, overhead := a.aead.NonceSize(), a.aead.Overhead()
	if len(sealed) < ns+overhead {
		return nil, ErrCiphertextTooShort
	}
	nonce, ciphertext := sealed[:ns], sealed[ns:]
	plaintext, err := a.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// GCM open failure = authentication failure (tamper / wrong key), not a
		// format error. Wrap the sentinel so callers can map it to 500 + alert.
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	return plaintext, nil
}

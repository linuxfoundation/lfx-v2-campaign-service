// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ConnectionReader reads a project's singleton connection for a provider.
type ConnectionReader interface {
	// Get returns the project's connection for the provider, or ErrNotFound.
	Get(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error)
}

// ConnectionWriter mutates connections. Every mutation is scoped to one
// project+provider (the singleton).
type ConnectionWriter interface {
	// Create inserts the project's connection for the provider. Returns
	// ErrConflict if one already exists (singleton violation).
	Create(ctx context.Context, c *model.Connection) (*model.Connection, error)

	// Update replaces the connection's config, gating on expectedVersion
	// (optimistic concurrency). It does NOT touch credentials. Returns
	// ErrNotFound if absent, ErrPreconditionFailed on version mismatch.
	Update(ctx context.Context, c *model.Connection, expectedVersion int64) (*model.Connection, error)

	// SetCredential replaces only the encrypted credential blob and bumps the
	// version. Separate from Update so credential replacement is independently
	// permissioned/audited. Returns the updated connection so the handler can
	// emit the new ETag (otherwise the client's next If-Match would be stale).
	SetCredential(ctx context.Context, projectID string, provider model.Provider, ciphertext []byte, by *model.Actor) (*model.Connection, error)

	// Delete soft-deletes the connection (status = deleted).
	Delete(ctx context.Context, projectID string, provider model.Provider) error
}

// ConnectionRepository is the full persistence port for connections.
type ConnectionRepository interface {
	ConnectionReader
	ConnectionWriter
}

// Encryptor encrypts and decrypts credential payloads at the application layer
// (AES-256-GCM). The key lives only in the application (from a k8s secret), not
// in the database.
type Encryptor interface {
	Encrypt(plaintext []byte) (ciphertext []byte, err error)
	Decrypt(ciphertext []byte) (plaintext []byte, err error)
}

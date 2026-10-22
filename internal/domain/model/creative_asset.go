// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"time"
)

// Creative-asset MIME types. These mirror the CHECK constraint on
// creative_assets.mime_type (migration 000028), and the upload contract's Enum will
// mirror them a third time when that endpoint lands. All must move together: each is
// the same PNG/JPEG allow-list enforced at a different layer (contract decode, handler
// validation, database).
const (
	// MimeTypePNG is the verified content type of a PNG upload.
	MimeTypePNG = "image/png"
	// MimeTypeJPEG is the verified content type of a JPEG upload.
	MimeTypeJPEG = "image/jpeg"
)

// CreativeAsset is an uploaded image, subordinate to a brief, that a Meta ad creative
// references by id. It holds the SOURCE BYTES, not a platform handle: Meta's image_hash
// is per-ad-account and only resolvable at dispatch, so the bytes must survive the gap
// between upload and campaign create. A brief may accumulate several assets over time.
//
// It is INSERT-ONLY. The bytes are immutable once stored, so there is no Version or
// Updated* field (unlike Brief / Campaign / CampaignAudience). A re-upload of identical
// bytes to the same brief is resolved by the (BriefID, Checksum) uniqueness — the store
// returns the existing asset — rather than by mutating a row.
type CreativeAsset struct {
	ID        string
	ProjectID string
	BriefID   string
	// MimeType is the VERIFIED type, sniffed from Bytes by the upload handler rather than
	// trusted from the client's declared header. One of MimeTypePNG / MimeTypeJPEG.
	MimeType string
	// ByteSize is len(Bytes), stored explicitly so callers and metrics can read the size
	// without loading the bytes.
	ByteSize int64
	// Checksum is the lowercase-hex SHA-256 of Bytes: the dedupe key within a brief and the
	// idempotency key the Meta client uses to avoid re-uploading the same image to an account.
	Checksum string
	// Width and Height are the image's pixel dimensions, read from its header by the upload
	// handler (image.DecodeConfig) and persisted alongside the bytes so that every later
	// reader — the per-platform slot classifier above all — gets an image's shape without
	// detoasting and re-decoding a blob of up to 30 MiB.
	//
	// They are a PAIR and are either both set or both zero. Zero means UNKNOWN, not 0x0: it is
	// what an asset stored before this service recorded dimensions reads back as (migration
	// 000047 added the columns nullable precisely so those rows stay valid). A reader must
	// treat the zero pair as "cannot be classified" rather than as a tiny image, which would
	// report every pre-existing asset as failing every slot minimum.
	Width  int
	Height int
	// Bytes is the raw image. It is not a secret (unlike connection credentials) and is
	// stored in Postgres as BYTEA in plaintext.
	Bytes []byte
	// CreatedBy names whoever uploaded the asset. Nil means "not recorded" — an
	// unauthenticated write — never "nobody".
	CreatedBy json.RawMessage
	CreatedAt time.Time
}

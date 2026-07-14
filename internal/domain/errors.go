// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package domain holds the core domain model, port interfaces, and sentinel
// errors for the campaign service. It has no infrastructure dependencies.
package domain

import "errors"

// Sentinel errors returned by repositories and mapped to HTTP status codes at
// the service/handler boundary.
var (
	// ErrNotFound indicates the requested resource does not exist (or has been
	// soft-deleted). Maps to 404.
	ErrNotFound = errors.New("resource not found")

	// ErrConflict indicates a uniqueness violation — for connections, that the
	// project already holds a connection for this provider (singleton). Maps to
	// 409.
	ErrConflict = errors.New("resource already exists")

	// ErrStaleApproval indicates the approve→dispatch guard fired: the brief was
	// no longer approved at the expected version when the job was created (a
	// concurrent replace/archive committed in the window, or it lost approval).
	// It is distinct from ErrConflict (a uniqueness violation) because the client
	// remedy differs — refresh and re-approve, then retry — even though both map
	// to 409. Maps to 409.
	ErrStaleApproval = errors.New("brief is no longer approved at the expected version")

	// ErrPreconditionFailed indicates an optimistic-concurrency version
	// mismatch on a conditional update (stale If-Match). Maps to 412.
	ErrPreconditionFailed = errors.New("version precondition failed")
)

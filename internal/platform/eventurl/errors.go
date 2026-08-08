// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import "errors"

var (
	// ErrEventURLInvalid indicates a malformed or unsupported URL.
	ErrEventURLInvalid = errors.New("event URL is invalid")

	// ErrEventURLForbidden indicates the URL resolves to a forbidden address.
	ErrEventURLForbidden = errors.New("event URL resolves to a forbidden address")

	// ErrEventURLFetchFailed indicates the fetch operation failed.
	ErrEventURLFetchFailed = errors.New("event URL fetch failed")
)

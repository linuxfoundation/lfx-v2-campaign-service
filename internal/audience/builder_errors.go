// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Audience-builder failure vocabulary (LFXV2-2770)
//
// Sentinels for the failures the handler layer must map to a specific status rather
// than to a generic 500. They exist because the distinctions are not recoverable from
// a message: "this portal holds no such list" is a 404 an operator acts on, while a
// missing event name is a 400 about the URL they typed, and both would otherwise read
// as the same server fault.
//
// They live HERE, beside the transport-neutral results, rather than in the
// orchestration that raises them. The handler package already imports this one for
// those results, and importing the orchestration package instead would close a cycle
// through its tests -- which is how this arrangement was found. Keeping the vocabulary
// with the shapes also puts the whole contract between the two layers in one package.
// ---------------------------------------------------------------------------

var (
	// ErrInvalidRequest means the request itself was unusable — a missing list
	// reference, an empty selection. The caller must change it rather than retry.
	ErrInvalidRequest = errors.New("audience: invalid request")
	// ErrNoInclusionLists means a compose request selected nothing to include. Wraps
	// ErrInvalidRequest so a handler can map every bad request in one arm while a
	// caller that cares about this specific case can still match it.
	ErrNoInclusionLists = fmt.Errorf("%w: at least one inclusion list is required", ErrInvalidRequest)
	// ErrTooManyPreviewLists means a preview-count selected more lists than the
	// sweep budget allows. Wraps ErrInvalidRequest for the same reason
	// ErrNoInclusionLists does: it is the caller's request to change, not a
	// transient upstream condition to retry. Names the limit because the only
	// useful remedy is to select fewer.
	ErrTooManyPreviewLists = fmt.Errorf("%w: at most %d lists can be previewed at once", ErrInvalidRequest, PreviewMaxLists)
	// ErrListNotFound means the referenced list does not exist in this portal.
	ErrListNotFound = errors.New("audience: the portal holds no such list")
	// ErrEventPageUnavailable means this deployment cannot read event pages at all.
	ErrEventPageUnavailable = errors.New("audience discovery: event-page reading is not configured")
	// ErrEventNameUnresolved means the page was read but declared no event name.
	ErrEventNameUnresolved = errors.New("audience discovery: the page did not declare an event name")
)

// ErrComposePartial reports that the combined suppression list WAS created but the
// master was not.
//
// A sentinel because the caller must not offer a plain retry: the suppression list
// exists in the portal under its final name, so a retry either fails on the duplicate
// name or leaves a second one behind. The orphan is surfaced to the operator with a
// link instead.
var ErrComposePartial = errors.New("audience compose: the combined suppression list was created but the master list was not")

// ComposePartialError carries the orphaned suppression list alongside the cause.
type ComposePartialError struct {
	Suppression ComposedList
	Err         error
}

func (e *ComposePartialError) Error() string {
	return fmt.Sprintf("%v: %v", ErrComposePartial, e.Err)
}

func (e *ComposePartialError) Unwrap() []error { return []error{ErrComposePartial, e.Err} }

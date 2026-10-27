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
	// ErrBlankExclusionID rejects a whitespace-only exclusion rather than dropping it. Silently
	// composing a master with NO suppression when one was requested is the wrong failure on a
	// create path that is not idempotent.
	ErrBlankExclusionID = fmt.Errorf("%w: an exclusion list id cannot be blank", ErrInvalidRequest)
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

// ErrComposePartial reports that compose left behind platform state that a plain
// retry must not blindly duplicate: a suppression list that WAS created, a master
// create that is UNCONFIRMED (HubSpot may have created it), or both.
//
// A sentinel because the caller must not offer a plain retry: any list this
// describes may already exist in the portal under its final name, so a retry either
// fails on the duplicate name or leaves a second one behind. ComposePartialError
// carries the specifics an operator needs to reconcile before trying again.
var ErrComposePartial = errors.New("audience compose: left platform state that must be reconciled before retrying")

// ErrComposePortalUnconfirmed reports that a compose asked to RECORD its result could not
// establish which portal it would be composing into.
//
// Distinct from ErrComposePartial because it is the opposite situation: nothing was created,
// so a plain retry is not only safe but correct. It exists at all because recording without
// provable provenance is refused — a `built` audience with an empty portal is one the dispatch
// guard rejects permanently, and no retry can repair a stored empty value.
var ErrComposePortalUnconfirmed = errors.New("audience compose: could not confirm which HubSpot portal the lists would be created in")

// ComposePartialError carries the orphaned suppression list alongside the cause.
//
// MasterName is set only when the MASTER create itself is unconfirmed (HubSpot may
// have created it despite the error) -- a definite master-create failure leaves it
// empty, since nothing needs reconciling on that side. An unconfirmed create has no
// id to give, so the deterministic name is the only reconcile key available, exactly
// as it is for Suppression.
//
// SuppressionUnconfirmed marks the same distinction on the suppression side: it is
// set only when the SUPPRESSION create itself is unconfirmed, in which case
// Suppression carries just its deterministic Name (ListID empty, since HubSpot never
// confirmed one). Without this marker, an unconfirmed suppression create is
// indistinguishable from a confirmed one that merely lacks a size -- both would
// otherwise present Suppression.Name set and ListID empty as "created".
// Master is set only on the RECORD-failed shape: both lists were confirmed created, and
// the attempt to record them as the brief's audience failed. It is the one partial shape
// where the master exists and is usable — the operator can attach it by hand — so it
// carries the whole list rather than just a reconcile name. Every other shape leaves it
// zero-valued, because on those the master either definitely does not exist or is
// unconfirmed and has only MasterName to offer.
type ComposePartialError struct {
	Suppression            ComposedList
	SuppressionUnconfirmed bool
	MasterName             string
	Master                 *ComposedList
	Err                    error
}

func (e *ComposePartialError) Error() string {
	return fmt.Sprintf("%v: %v", ErrComposePartial, e.Err)
}

func (e *ComposePartialError) Unwrap() []error { return []error{ErrComposePartial, e.Err} }

// ErrStandingListConflict means a list already holds the name of one of the service's
// standing suppression lists (the project's standard list, or an edition's current
// registrants) but is not a list the service can safely reuse: not DYNAMIC, or selecting
// something other than what the name promises. List names are unique per portal, so the
// service can neither reuse it nor create its own beside it; an operator must rename or
// remove the conflicting list in HubSpot.
var ErrStandingListConflict = errors.New("audience: a list holding a standing suppression list's name cannot be reused")

// StandingListConflictError names the conflicting list and why it cannot be reused. Name
// and ListID are the portal's own; Reason is this service's text and never upstream
// material.
type StandingListConflictError struct {
	Name   string
	ListID string
	Reason string
}

func (e *StandingListConflictError) Error() string {
	return fmt.Sprintf("HubSpot list %q (id %s) %s; rename or remove it in HubSpot so the service can use its own list under that name", e.Name, e.ListID, e.Reason)
}

func (e *StandingListConflictError) Unwrap() error { return ErrStandingListConflict }

// ExclusionOverlapError is a selection that would both send to and suppress one list, found
// only once the suppressions were RESOLVED: the standing standard list stands in for the
// selected standard lists, and current_registrants_event adds the edition's registrants list,
// so a list the caller included can turn up among the exclusions without appearing in the
// request's own suppression ids. Wraps ErrInvalidRequest: the request must change. Role says
// how the list became an exclusion; Name is the portal's own and may be empty.
type ExclusionOverlapError struct {
	ListID string
	Name   string
	Role   string
}

func (e *ExclusionOverlapError) Error() string {
	list := "list " + e.ListID
	if e.Name != "" {
		list = fmt.Sprintf("list %s (%q)", e.ListID, e.Name)
	}
	return fmt.Sprintf("%s is %s, and it is also a list this send goes to; a list cannot be both sent to and suppressed — remove it from the inclusion lists or drop the selection that suppresses it", list, e.Role)
}

func (e *ExclusionOverlapError) Unwrap() error { return ErrInvalidRequest }

// FieldError is a request field whose value can never work, with the reason in the caller's
// terms. Wraps ErrInvalidRequest; the handler answers 400 with Error(), which names the field,
// instead of the generic invalid-request text that names neither.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }

func (e *FieldError) Unwrap() error { return ErrInvalidRequest }

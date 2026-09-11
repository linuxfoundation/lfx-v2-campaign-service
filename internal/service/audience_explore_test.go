// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// audienceExploreErr is a switch whose ARM ORDER is load-bearing, and two of its arms
// say so in prose ("Above the ErrNotFound/ErrConnectionNotUsable arms", "NOT the 503
// below") without anything enforcing it. The cases below are the ones that actually
// overlap: creds.go:617 wraps ErrSystemConnectionMissing ALONGSIDE ErrNotFound, so a
// single production error matches two arms and only position decides which message an
// operator reads. Reordering the switch keeps compiling and keeps every other test
// green -- these are what fail.
//
// Scope, so the next reader does not re-derive it: only the SYSTEM-connection arms
// genuinely overlap another arm today, so only those two cases detect a reorder.
// ErrCredentialDecryptionFailed carries no second sentinel (creds.go:876 wraps it in
// notCreated, a plain wrapper), so its "NOT the 503 below" comment guards against a
// FUTURE edit that co-wraps a connection error rather than describing a present
// overlap -- moving that arm today changes no behaviour, and this test passing under
// that move is correct, not a gap. The remaining cases pin each arm's mapping, which
// is what was at 0% coverage.
func TestAudienceExploreErrClassification(t *testing.T) {
	// The co-wrapped shape creds.go really produces, not a synthetic single error.
	systemMissing := fmt.Errorf("resolve hubspot: %w",
		errors.Join(domain.ErrSystemConnectionMissing, domain.ErrNotFound))
	systemNotUsable := fmt.Errorf("resolve hubspot: %w",
		errors.Join(domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable))

	tests := []struct {
		name     string
		err      error
		wantCode string
		// wantType pins WHICH contract error type is returned: the code alone does not,
		// since 503 is produced by three different arms for three different reasons.
		wantType any
		// wantMsgContains is what actually separates the overlapping arms: three
		// different arms return 503/ConnServiceUnavailableError, so type+code alone
		// cannot detect a reorder. The MESSAGE is the operator-facing difference the
		// arm comments are about, so that is what gets pinned.
		wantMsgContains string
		why             string
	}{
		{
			name:            "a missing list is a 404",
			err:             fmt.Errorf("read list: %w", audience.ErrListNotFound),
			wantCode:        "404",
			wantType:        &explore.NotFoundError{},
			wantMsgContains: "no list in this project's HubSpot portal",
			why:             "the portal genuinely holds no such list",
		},
		{
			name:            "an over-budget preview is a 400, not a 500",
			err:             audience.ErrTooManyPreviewLists,
			wantCode:        "400",
			wantType:        &explore.BadRequestError{},
			wantMsgContains: "could not be satisfied as given",
			why:             "wraps ErrInvalidRequest; retrying the same payload can never succeed",
		},
		{
			name:            "an unresolved event name is a 400",
			err:             fmt.Errorf("discover: %w", audience.ErrEventNameUnresolved),
			wantCode:        "400",
			wantType:        &explore.BadRequestError{},
			wantMsgContains: "could not be satisfied as given",
			why:             "the fix is a different event URL, not waiting for the service",
		},
		{
			name:            "an unconfigured event-page reader is a 503",
			err:             audience.ErrEventPageUnavailable,
			wantCode:        "503",
			wantType:        &explore.ConnServiceUnavailableError{},
			wantMsgContains: "cannot read event pages",
			why:             "the deployment cannot read event pages at all",
		},
		{
			name:            "a missing SYSTEM connection outranks the plain not-found arm",
			err:             systemMissing,
			wantCode:        "503",
			wantType:        &explore.ConnServiceUnavailableError{},
			wantMsgContains: "the shared LF HubSpot connection",
			why:             "matches BOTH arms; below the ErrNotFound arm it would tell the project to reconnect HubSpot, a repair nobody on the project can perform",
		},
		{
			name:            "an unusable SYSTEM connection outranks the plain not-usable arm",
			err:             systemNotUsable,
			wantCode:        "503",
			wantType:        &explore.ConnServiceUnavailableError{},
			wantMsgContains: "the shared LF HubSpot connection",
			why:             "same overlap as above, via ErrConnectionNotUsable",
		},
		{
			name:            "failed credential decryption is a 500, never the 503 below it",
			err:             fmt.Errorf("decrypt: %w", domain.ErrCredentialDecryptionFailed),
			wantCode:        "500",
			wantType:        &explore.InternalServerError{},
			wantMsgContains: "could not be decrypted",
			why:             "retrying cannot help: a corrupt row or a rotated key needs an operator",
		},
		{
			name:            "no connection for this project is a 503, not a 404",
			err:             fmt.Errorf("resolve: %w", domain.ErrNotFound),
			wantCode:        "503",
			wantType:        &explore.ConnServiceUnavailableError{},
			wantMsgContains: "this project has no usable HubSpot connection",
			why:             "the list may well exist; what is unavailable is the connection to look",
		},
		{
			name:            "an unrecognised failure is a 500",
			err:             errors.New("hubspot returned something nobody mapped"),
			wantCode:        "500",
			wantType:        &explore.InternalServerError{},
			wantMsgContains: "could not complete this request against HubSpot",
			why:             "the default arm",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := audienceExploreErr(context.Background(), "test-op", "tlf", tc.err)

			if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", tc.wantType) {
				t.Fatalf("wrong error TYPE: got %T, want %T\nwhy this arm: %s", got, tc.wantType, tc.why)
			}
			code, msg := fieldsOf(t, got)
			if code != tc.wantCode {
				t.Errorf("wrong code: got %s, want %s\nwhy this arm: %s", code, tc.wantCode, tc.why)
			}
			if !strings.Contains(msg, tc.wantMsgContains) {
				t.Errorf("wrong arm matched -- message does not mention %q\ngot:  %s\nwhy this arm: %s",
					tc.wantMsgContains, msg, tc.why)
			}
		})
	}
}

// fieldsOf reads the Code and Message fields common to every contract error type above.
func fieldsOf(t *testing.T, err error) (code, msg string) {
	t.Helper()
	switch e := err.(type) {
	case *explore.NotFoundError:
		return e.Code, e.Message
	case *explore.BadRequestError:
		return e.Code, e.Message
	case *explore.ConnServiceUnavailableError:
		return e.Code, e.Message
	case *explore.InternalServerError:
		return e.Code, e.Message
	default:
		t.Fatalf("unexpected error type %T", err)
		return "", ""
	}
}

// composeErr must NOT flatten a partial compose into a generic failure. Compose is
// not idempotent: if the suppression list was created and the master was not, a
// caller told only "it failed" offers a Retry, and that retry either collides on the
// suppression list's final name or leaves a SECOND orphan in a production portal.
// The orphan's id has to survive into the response for anyone to reconcile it.
func TestComposeErrPreservesThePartialStateAndItsOrphanID(t *testing.T) {
	partial := &audience.ComposePartialError{
		Suppression: audience.ComposedList{ListRow: audience.ListRow{
			ListID:     "998877",
			Name:       "KubeCon NA 2026 — Combined Suppression",
			HubSpotURL: "https://app.hubspot.com/contacts/123/objectLists/998877",
		}},
		Err: errors.New("create master: hubspot 500"),
	}

	got := composeErr(context.Background(), "tlf", fmt.Errorf("compose: %w", partial))

	res, ok := got.(*explore.AudienceComposePartialError)
	if !ok {
		t.Fatalf("a partial compose was flattened to %T; the orphaned list id is then lost and the operator is invited to retry", got)
	}
	if res.Code != "409" {
		t.Errorf("want 409 (conflict needing reconciliation), got %s", res.Code)
	}
	if res.Suppression == nil || res.Suppression.ListID != "998877" {
		t.Fatalf("the orphaned suppression list id must survive into the response; got %+v", res.Suppression)
	}
	if !strings.Contains(res.Message, "do not simply retry") {
		t.Errorf("the message must warn against a blind retry, since compose is not idempotent; got %q", res.Message)
	}
}

// Anything that is NOT a partial falls through to the shared classifier rather than
// being reported as a conflict.
func TestComposeErrDelegatesNonPartialFailures(t *testing.T) {
	got := composeErr(context.Background(), "tlf", audience.ErrNoInclusionLists)

	if _, bad := got.(*explore.AudienceComposePartialError); bad {
		t.Fatalf("a non-partial failure was reported as a partial compose: %T", got)
	}
	if _, ok := got.(*explore.BadRequestError); !ok {
		t.Fatalf("want the classifier's 400 arm for ErrNoInclusionLists, got %T", got)
	}
}

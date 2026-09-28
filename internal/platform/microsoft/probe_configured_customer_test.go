// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestListAdAccounts_AConfiguredCustomerRefusedIsClaimedByItsOwnPredicate pins the finding this
// predicate was added for (LFXV2-2665).
//
// A configured customer_id that does not exist, or that these credentials cannot reach, comes
// back from AccountsInfo/Query as an ordinary 400. Before the marker that 400 matched NEITHER
// ProbeCredentialRejected (401/403 only) nor ProbeInconclusive (429/408/5xx only), so probeClass
// fell to its default arm and the connection test answered domain.ErrServiceDefect — a typed 500
// that pages us — for a stale value on the operator's own connection row. customer_id is
// settable through the connection config API and access to a customer can be revoked long after
// the value was stored, so this is a reachable state and not a theoretical one.
//
// The assertions run in the order that matters: the marker first, then the two standard
// predicates, because a fix that set the marker while ALSO letting the rejection arm claim the
// error would still render "microsoft ads rejected the stored credential" and send the operator
// to re-authorise a credential Microsoft honoured well enough to answer with a 400.
func TestListAdAccounts_AConfiguredCustomerRefusedIsClaimedByItsOwnPredicate(t *testing.T) {
	c := newCustomerClient(t, AccountConfig{CustomerID: "9988776"}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/User/Query") {
			t.Error("User/Query was called; a configured customer id skips role discovery")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"Errors":[{"Code":1100,"ErrorCode":"CustomerNotFound"}]}`)
	})

	_, err := c.ListAdAccounts(context.Background())
	if err == nil {
		t.Fatal("ListAdAccounts returned no error for a 400 on AccountsInfo/Query")
	}
	if !ProbeConfiguredCustomerRejected(err) {
		t.Fatalf("ProbeConfiguredCustomerRejected(%v) = false, want true: a 400 about the "+
			"configured customer id matches no other predicate, so probeClass raises "+
			"ErrServiceDefect and pages us about a field the operator can correct", err)
	}
	if ProbeCredentialRejected(err) {
		t.Errorf("ProbeCredentialRejected(%v) = true, want false: Microsoft accepted the "+
			"credential far enough to answer, and a rejection verdict sends the operator to "+
			"re-authorise a connection whose credential is fine", err)
	}
	if ProbeInconclusive(err) {
		t.Errorf("ProbeInconclusive(%v) = true, want false: the platform answered, so this is "+
			"a verdict and not an outage to wait out", err)
	}
}

// TestListAdAccounts_ADiscoveredCustomerRefusedStaysADefect pins the other half, and it is the
// half that keeps the fix honest.
//
// With no customer_id configured, the id in the request is one this client read out of
// User/Query moments earlier and the request body is otherwise composed entirely here. A 400
// then is not the operator's value being refused — it is the shape of a request only this
// service builds, or a contract that moved, and it must keep reaching probeClass's default arm
// so it keeps paging us. Marking both provenances would have told an operator to repair a field
// that is not broken and silenced the one class of 400 that genuinely is ours.
func TestListAdAccounts_ADiscoveredCustomerRefusedStaysADefect(t *testing.T) {
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/User/Query") {
			_, _ = io.WriteString(w, `{"CustomerRoles":[{"CustomerId":1111111,"RoleId":41}]}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"Errors":[{"Code":1100,"ErrorCode":"CustomerNotFound"}]}`)
	})

	_, err := c.ListAdAccounts(context.Background())
	if err == nil {
		t.Fatal("ListAdAccounts returned no error for a 400 on AccountsInfo/Query")
	}
	if ProbeConfiguredCustomerRejected(err) {
		t.Errorf("ProbeConfiguredCustomerRejected(%v) = true for a customer this client "+
			"discovered itself; the connection row supplied nothing here, so there is no "+
			"operator field to send anyone to", err)
	}
	if ProbeCredentialRejected(err) || ProbeInconclusive(err) {
		t.Errorf("a discovered-customer 400 must match neither standard predicate so it reaches "+
			"the service-defect arm; rejected=%v inconclusive=%v",
			ProbeCredentialRejected(err), ProbeInconclusive(err))
	}
}

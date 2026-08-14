// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// A Reddit connection saved before the conversion_pixel_id column existed carries no pixel,
// and Reddit rejects every create without one. The dispatch must refuse BEFORE contacting
// the platform and must point the operator at the CONNECTION, which is where the fix lives —
// not at the campaign they were editing, which is where they are looking.
//
// The claim contract matters as much as the message: nothing was created upstream, so this
// has to be a notCreated error that RELEASES the claim. Reported as created-but-unconfirmed
// it would strand a pending row that nothing reaps, and the slot would stay blocked forever.
func TestReddit_MissingConversionPixelIsRefused(t *testing.T) {
	conn := activeRedditConn(goodRedditCreds)
	// Exactly the shape of a pre-migration row: the key is simply absent.
	conn.ProviderConfig = map[string]string{}

	d := NewRedditDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})
	cfg := json.RawMessage(`{"redditConfig":{"eventName":"KubeCon","budgetUsd":500,"registrationUrl":"https://example.com/reg","geoTargets":["us"],"keywords":["k8s"],"startDate":"2027-01-04","endDate":"2027-01-18"}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err == nil {
		t.Fatalf("a connection with no conversion pixel must be refused, got campaign %+v", camp)
	}
	if !strings.Contains(err.Error(), "conversion pixel id") {
		t.Errorf("error %q does not name the missing pixel", err.Error())
	}
	if !strings.Contains(err.Error(), "connection") {
		t.Errorf("error %q does not point the operator at the connection", err.Error())
	}

	// The claim must be RELEASED: nothing exists upstream, so a retry after the operator
	// adds the pixel has to be able to take the slot.
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a pre-create refusal must be a no-upstream-create error so the claim is released; got %T", err)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Off (the default, and any value but "true") answers ErrTargetingLookupUnsupported without
// contacting Microsoft; on lists the account's remarketing lists on the create's own connection.
func TestMicrosoftRemarketingLists_GateAndRead(t *testing.T) {
	api := &msAudAPI{lookupBody: `{"Audiences":[{"Id":8394012,"Type":"RemarketingList","Name":"KubeCon visitors","Scope":"Account","MembershipDuration":30}]}`}
	d := newMSAudDispatcher(t, api)
	for _, v := range []string{"", "TRUE", "1"} {
		t.Setenv(constants.EnvMicrosoftRemarketingListsEnabled, v)
		_, called, err := d.ListMicrosoftRemarketingLists(context.Background(), "cncf", model.ProviderMicrosoftAds)
		if !errors.Is(err, domain.ErrTargetingLookupUnsupported) || called || !strings.Contains(err.Error(), constants.EnvMicrosoftRemarketingListsEnabled) {
			t.Fatalf("flag %q: called=%v err=%v, want not called and ErrTargetingLookupUnsupported naming the flag", v, called, err)
		}
	}
	if n := len(api.snapshot()); n != 0 {
		t.Fatalf("%d Microsoft calls with the flag off; want 0", n)
	}
	t.Setenv(constants.EnvMicrosoftRemarketingListsEnabled, "true")
	res, called, err := d.ListMicrosoftRemarketingLists(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if err != nil || !called {
		t.Fatalf("flag on: called=%v err=%v", called, err)
	}
	if res.AccountID != "1234567" || len(res.Lists) != 1 || res.Lists[0].ID != "8394012" || res.Lists[0].Name != "KubeCon visitors" ||
		res.Lists[0].MembershipDuration == nil || *res.Lists[0].MembershipDuration != 30 {
		t.Fatalf("res = %+v", res)
	}
	if calls := api.snapshot(); len(calls) != 1 || calls[0] != "Audiences/QueryByIds" {
		t.Fatalf("calls = %v", calls)
	}
}

// An answer the client cannot believe reaches the caller as an error after a real call, with no
// result.
func TestMicrosoftRemarketingLists_UnbelievableAnswerIsAnError(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftRemarketingListsEnabled, "true")
	d := newMSAudDispatcher(t, &msAudAPI{lookupBody: `{}`})
	res, called, err := d.ListMicrosoftRemarketingLists(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if !errors.Is(err, microsoft.ErrRemarketingListsMalformed) || !called || res != nil {
		t.Fatalf("got (%+v, %v, %v)", res, called, err)
	}
}

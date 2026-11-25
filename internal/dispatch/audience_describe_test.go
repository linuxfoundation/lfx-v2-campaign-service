// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// describeServer is a fake portal: a master list referencing two others, a registrants list, a
// static list, a missing one and a broken one; plus the contact property one rule names.
func describeServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case "/crm/v3/lists/100":
			_, _ = io.WriteString(w, `{"list":{"listId":"100","name":"KubeCon NA 2026 Master","processingType":"DYNAMIC","size":4200,
				"filterBranch":{"filterBranchType":"OR","filters":[],"filterBranches":[
					{"filterBranchType":"AND","filters":[{"filterType":"IN_LIST","listId":"101","operator":"IN_LIST"},{"filterType":"IN_LIST","listId":"900","operator":"NOT_IN_LIST"}],"filterBranches":[]},
					{"filterBranchType":"AND","filters":[{"filterType":"PROPERTY","property":"lifecyclestage","operation":{"operationType":"ENUMERATION","operator":"IS_ANY_OF","values":["subscriber","lead"]}}],"filterBranches":[]}
				]}}}`)
		case "/crm/v3/lists/101":
			_, _ = io.WriteString(w, `{"list":{"listId":"101","name":"KubeCon NA 2025 Registrants","processingType":"DYNAMIC","size":9000}}`)
		case "/crm/v3/lists/900":
			_, _ = io.WriteString(w, `{"list":{"listId":"900","name":"LFX Standard Suppressions - tlf","processingType":"DYNAMIC"}}`)
		case "/crm/v3/lists/200":
			_, _ = io.WriteString(w, `{"list":{"listId":"200","name":"Booth scans","processingType":"MANUAL","size":120}}`)
		case "/crm/v3/lists/404":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"status":"error","message":"not found"}`)
		case "/crm/v3/lists/500":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"status":"error","message":"forbidden"}`)
		case "/crm/v3/properties/contacts/lifecyclestage":
			_, _ = io.WriteString(w, `{"name":"lifecyclestage","label":"Lifecycle Stage","type":"enumeration","options":[{"label":"Subscriber","value":"subscriber"},{"label":"Lead","value":"lead"}]}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/contacts/v1/lists/") {
				// The legacy name fallback listName uses on a v3 miss.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func describeExplorer(srvURL string) *AudienceExplorer {
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	return NewAudienceExplorer(NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srvURL)), nil, nil, nil)
}

func TestDescribeLists_NamesReferencesAndKeepsRequestOrder(t *testing.T) {
	var calls int32
	srv := describeServer(t, &calls)
	defer srv.Close()

	got, err := describeExplorer(srv.URL).DescribeLists(context.Background(), "proj-1", []string{"100", "200", "404", "100"})
	require.NoError(t, err)
	require.Len(t, got, 3, "a duplicate id is described once")

	master := got[0]
	assert.Equal(t, "100", master.ListID)
	assert.Equal(t, audience.DescribeStatusDescribed, master.Status)
	assert.Equal(t, 4200, master.Size)
	assert.True(t, master.Complete)
	require.Len(t, master.Groups, 2)
	assert.Equal(t, []string{
		"Is a member of the list “KubeCon NA 2025 Registrants”",
		"Is not a member of the list “LFX Standard Suppressions - tlf”",
	}, master.Groups[0].Conditions)
	assert.Equal(t, []string{"Lifecycle Stage is any of “Subscriber” or “Lead”"}, master.Groups[1].Conditions)
	assert.Contains(t, master.Summary, "any of 2 groups")
	assert.NotEmpty(t, master.HubSpotURL)

	assert.Equal(t, audience.DescribeStatusStatic, got[1].Status)
	assert.Equal(t, 120, got[1].Size)
	assert.Equal(t, audience.DescribeStatusNotFound, got[2].Status)
	assert.Equal(t, -1, got[2].Size)
}

// One unreadable list must not hide what the others mean.
func TestDescribeLists_OneUnreadableListDoesNotFailTheBatch(t *testing.T) {
	var calls int32
	srv := describeServer(t, &calls)
	defer srv.Close()

	got, err := describeExplorer(srv.URL).DescribeLists(context.Background(), "proj-1", []string{"500", "200"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, audience.DescribeStatusUnreadable, got[0].Status)
	assert.Equal(t, audience.DescribeStatusStatic, got[1].Status)
}

// When NOTHING could be read the answer is about the connection, so it is an error the status
// mapping can classify -- not a batch of "unreadable" rows that hide a broken token.
func TestDescribeLists_AllUnreadableIsAnError(t *testing.T) {
	var calls int32
	srv := describeServer(t, &calls)
	defer srv.Close()

	_, err := describeExplorer(srv.URL).DescribeLists(context.Background(), "proj-1", []string{"500"})
	require.Error(t, err)
}

func TestDescribeLists_RefusesAnOversizedBatchBeforeAnyRequest(t *testing.T) {
	var calls int32
	srv := describeServer(t, &calls)
	defer srv.Close()

	ids := make([]string, 0, audience.DescribeMaxLists+1)
	for i := 0; i <= audience.DescribeMaxLists; i++ {
		ids = append(ids, "l"+strings.Repeat("x", i+1))
	}
	_, err := describeExplorer(srv.URL).DescribeLists(context.Background(), "proj-1", ids)
	var field *audience.FieldError
	require.ErrorAs(t, err, &field)
	assert.Zero(t, atomic.LoadInt32(&calls), "a refused batch must not reach the portal")
}

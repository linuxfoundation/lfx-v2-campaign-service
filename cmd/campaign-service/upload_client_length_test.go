// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	goahttp "goa.design/goa/v3/http"

	briefsc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/client"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/middleware"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/uploadclient"
)

// These tests close issue #183 END TO END: the REAL generated briefs client, over a real TCP
// connection and net/http transport, against the REAL buildHandler chain (MaxBodyBytes and
// UploadAdmission with production constants). The defect was invisible to any narrower test --
// goa's encoder looks correct in isolation, and the middleware prices correctly in isolation --
// because it lived in what the transport does with a body whose length the encoder never set.
//
// Only the mux is substituted, by a handler that records what the server observed and parks
// the request so admission concurrency is measured deterministically rather than by timing.

// uploadObservation is what the parked inner handler saw for one admitted request.
type uploadObservation struct {
	contentLength    int64
	transferEncoding []string
}

type uploadHarness struct {
	host    string
	entered chan uploadObservation
	release func()
}

func startUploadHarness(t *testing.T) *uploadHarness {
	t.Helper()
	entered := make(chan uploadObservation, 8)
	releaseCh := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseCh) }) }

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- uploadObservation{contentLength: r.ContentLength, transferEncoding: r.TransferEncoding}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-releaseCh:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"6ba7b811-9dad-11d1-80b4-00c04fd430c8","project_id":"cncf",`+
			`"brief_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","mime_type":"image/png",`+
			`"byte_size":3072,"checksum":"00","created":"true"}`)
	})
	srv := httptest.NewServer(buildHandler(inner, &config.Config{}, middleware.NewInflightTracker()))
	t.Cleanup(srv.Close)
	// Registered after srv.Close, so it runs first: parked handlers return before Close waits.
	t.Cleanup(release)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &uploadHarness{host: u.Host, entered: entered, release: release}
}

// statusDoer records the status of every response, so a shed upload is identified by the 503
// the admission middleware wrote rather than by whatever error the client decoder makes of it.
type statusDoer struct {
	client *http.Client
	mu     sync.Mutex
	codes  []int
}

func (d *statusDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.client.Do(req)
	if err == nil {
		d.mu.Lock()
		d.codes = append(d.codes, resp.StatusCode)
		d.mu.Unlock()
	}
	return resp, err
}

func (d *statusDoer) count(code int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.codes {
		if c == code {
			n++
		}
	}
	return n
}

// smallUpload is an ordinary upload: well inside the floor, so its declared price is 8 MiB.
func smallUpload() *briefsvc.UploadCreativeAssetPayload {
	token := "test-token"
	return &briefsvc.UploadCreativeAssetPayload{
		BearerToken: &token,
		ProjectID:   "cncf",
		BriefID:     "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		ContentType: "image/png",
		Bytes:       strings.Repeat("A", 4096),
	}
}

func newUploadEndpoint(h *uploadHarness, enc func(*http.Request) goahttp.Encoder, doer *statusDoer) func(context.Context, any) (any, error) {
	c := briefsc.NewClient("http", h.host, doer, enc, goahttp.ResponseDecoder, false)
	return c.UploadCreativeAsset()
}

// TestUploadClient_DeclaredLengthUploadsRunConcurrently is the regression test #183 asked for:
// with uploadclient.RequestEncoder, the generated client declares a positive Content-Length, the
// server prices each upload by size, and two concurrent uploads that fit the budget are BOTH
// admitted while the other is still in flight.
//
// Mutation check: delete the ContentLength assignment in uploadclient's Encode and this test
// fails -- the transport falls back to chunked, the server sees -1, each upload is charged the
// whole budget, and the second is shed with 503 while the first is parked.
func TestUploadClient_DeclaredLengthUploadsRunConcurrently(t *testing.T) {
	// Independent precondition: two small declared uploads must fit together, or this test
	// could not distinguish "serialized" from "over budget".
	if w := constants.UploadAdmissionWeightFor(64 << 10); 2*w > constants.UploadAdmissionBudgetBytes {
		t.Fatalf("two 64 KiB declared uploads (%d each) exceed the budget %d", w, constants.UploadAdmissionBudgetBytes)
	}

	h := startUploadHarness(t)
	doer := &statusDoer{client: &http.Client{Timeout: 30 * time.Second}}
	upload := newUploadEndpoint(h, uploadclient.RequestEncoder, doer)

	const concurrent = 2
	results := make(chan error, concurrent)
	for range concurrent {
		go func() {
			_, err := upload(context.Background(), smallUpload())
			results <- err
		}()
	}

	// Both must be INSIDE the handler at once. A result arriving first means one finished --
	// shed -- while the other still held its permit: the serialized behaviour of #183.
	var seen []uploadObservation
	timeout := time.After(10 * time.Second)
	for len(seen) < concurrent {
		select {
		case obs := <-h.entered:
			seen = append(seen, obs)
		case err := <-results:
			h.release()
			t.Fatalf("an upload completed while another was parked (err=%v, 503s=%d): uploads were "+
				"serialized, so the client is not declaring its length", err, doer.count(http.StatusServiceUnavailable))
		case <-timeout:
			h.release()
			t.Fatalf("only %d of %d uploads were admitted concurrently", len(seen), concurrent)
		}
	}

	for i, obs := range seen {
		if obs.contentLength <= 0 {
			t.Errorf("upload %d: server observed ContentLength=%d (Transfer-Encoding %v), want a "+
				"positive declared length", i+1, obs.contentLength, obs.transferEncoding)
		}
	}

	h.release()
	for range concurrent {
		if err := <-results; err != nil {
			t.Errorf("upload failed: %v", err)
		}
	}
	if n := doer.count(http.StatusServiceUnavailable); n != 0 {
		t.Errorf("%d upload(s) were shed with 503, want none", n)
	}
}

// TestUploadClient_UndeclaredLengthIsStillChargedTheWholeBudget pins that the fix is on the
// CLIENT and the server guard is unchanged: the same generated client with goa's stock encoder
// sends the upload chunked, the server sees an unknown length, prices it at the worst case, and
// a second concurrent upload is shed while the first holds the budget.
func TestUploadClient_UndeclaredLengthIsStillChargedTheWholeBudget(t *testing.T) {
	h := startUploadHarness(t)
	doer := &statusDoer{client: &http.Client{Timeout: 30 * time.Second}}
	upload := newUploadEndpoint(h, goahttp.RequestEncoder, doer)

	first := make(chan error, 1)
	go func() {
		_, err := upload(context.Background(), smallUpload())
		first <- err
	}()

	select {
	case obs := <-h.entered:
		if obs.contentLength != -1 {
			t.Errorf("stock encoder: server observed ContentLength=%d, want -1 (chunked); if goa now "+
				"declares the length, this test no longer exercises the undeclared path", obs.contentLength)
		}
	case err := <-first:
		t.Fatalf("first undeclared upload finished before it was parked: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first undeclared upload was never admitted")
	}

	// The budget is now held by one undeclared upload. A second must be shed, not admitted.
	_, err := upload(context.Background(), smallUpload())
	if err == nil {
		t.Error("second undeclared upload succeeded while the first held the budget")
	}
	if n := doer.count(http.StatusServiceUnavailable); n != 1 {
		t.Errorf("503s = %d, want exactly 1: an undeclared upload must be charged the whole budget", n)
	}
	select {
	case obs := <-h.entered:
		t.Errorf("a second undeclared upload entered the handler concurrently (%+v)", obs)
	default:
	}

	h.release()
	if err := <-first; err != nil {
		t.Errorf("first undeclared upload: %v", err)
	}
}

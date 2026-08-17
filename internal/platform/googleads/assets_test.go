// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// assetServer answers assets:mutate with a well-formed asset resource and records every
// decoded body, mirroring captureServer but returning the assets/{id} shape uploadImageAsset
// validates for. A decode failure records nil rather than failing in the handler goroutine,
// where t.Fatalf would only kill the goroutine and half-finish the exchange.
func assetServer(t *testing.T, bodies *[]map[string]any, paths *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		*paths = append(*paths, r.URL.Path)
		*bodies = append(*bodies, decoded)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/assets/444"}]}`))
	}))
}

// firstAssetCreate pulls operations[0].create out of the single captured assets:mutate body,
// failing if none was seen — the shared shape every wire-shape assertion below starts from.
func firstAssetCreate(t *testing.T, bodies []map[string]any, paths []string) map[string]any {
	t.Helper()
	for i, p := range paths {
		if !strings.HasSuffix(p, "assets:mutate") {
			continue
		}
		ops, _ := bodies[i]["operations"].([]any)
		if len(ops) == 0 {
			t.Fatal("assets:mutate carried no operations")
		}
		op, _ := ops[0].(map[string]any)
		create, _ := op["create"].(map[string]any)
		if create == nil {
			t.Fatal("assets:mutate operation carried no create")
		}
		return create
	}
	t.Fatalf("no assets:mutate seen, paths = %v", paths)
	return nil
}

// The wire contract: the image goes out as standard-base64 imageAsset.data, and NOTHING
// else. A `type` would be rejected (output-only on create) and a `name` would invent a
// DUPLICATE_NAME concern image assets don't otherwise carry — both asserted absent so a
// later "helpful" addition of either fails here instead of at Google.
func TestUploadImageAssetSendsBase64ImageOnly(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := assetServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	rn, err := newDemandGenClient(t, srv.URL).uploadImageAsset(context.Background(), []byte("PNGDATA"))
	if err != nil {
		t.Fatalf("uploadImageAsset: %v", err)
	}
	if rn != "customers/1234567890/assets/444" {
		t.Errorf("resource name = %q, want customers/1234567890/assets/444", rn)
	}

	mu.Lock()
	defer mu.Unlock()
	create := firstAssetCreate(t, bodies, paths)
	img, _ := create["imageAsset"].(map[string]any)
	if img == nil {
		t.Fatalf("create carried no imageAsset, got %v", create)
	}
	wantData := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	if got := img["data"]; got != wantData {
		t.Errorf("imageAsset.data = %v, want standard-base64 %q", got, wantData)
	}
	if _, ok := img["type"]; ok {
		t.Error("imageAsset must not send a type on create — it is output-only and Google rejects it")
	}
	if _, ok := create["type"]; ok {
		t.Error("asset create must not send a type on create — it is output-only and Google rejects it")
	}
	if _, ok := create["name"]; ok {
		t.Error("asset create must not send a name — it invents a DUPLICATE_NAME concern image assets don't carry")
	}
}

// The empty-bytes guard, mirroring meta.uploadImage: an image role resolved to zero bytes is
// a caller bug, and sending an empty imageAsset would either 400 late or create a useless
// asset. Caught before any request goes out.
func TestUploadImageAssetRejectsEmptyBytes(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := assetServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	_, err := newDemandGenClient(t, srv.URL).uploadImageAsset(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error when called with no bytes")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 0 {
		t.Errorf("no request must be sent for empty bytes, got %v", paths)
	}
}

// A 2xx that names a DIFFERENT customer's asset must not be trusted: firstResourceName reads
// only a trailing id, so without validateResourceKind another account's id would be handed to
// G3's ad create as if it were ours. Reported UNCONFIRMED, not as a usable asset.
func TestUploadImageAssetRejectsForeignResource(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/9999999999/assets/444"}]}`))
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	rn, err := c.uploadImageAsset(context.Background(), []byte("PNGDATA"))
	if err == nil {
		t.Fatal("expected an error for a foreign-customer asset resource")
	}
	if rn != "" {
		t.Errorf("resource name = %q, want empty on rejection", rn)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a wrong-account 2xx must be reported UNCONFIRMED, got: %v", err)
	}
}

// A 2xx with an empty results array (no resource name) is UNCONFIRMED: the upload may have
// landed but cannot be named, so it must never be returned as a usable asset.
func TestUploadImageAssetTreatsMissingResourceAsUnconfirmed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	_, err := c.uploadImageAsset(context.Background(), []byte("PNGDATA"))
	if err == nil {
		t.Fatal("expected an error for a 2xx with no resource name")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 2xx with no resource name must be reported UNCONFIRMED, got: %v", err)
	}
}

// The asset create is non-idempotent: a mutating 429 must NOT be retried, because a blind
// retry of a create is exactly how a double-create happens. Asserted as a single POST to
// assets:mutate (the token exchange is a separate server and is not counted).
func TestUploadImageAssetDoesNotRetryMutating429(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	var mu sync.Mutex
	var posts int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	if _, err := c.uploadImageAsset(context.Background(), []byte("PNGDATA")); err == nil {
		t.Fatal("expected an error on a 429")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Errorf("assets:mutate POST count = %d, want 1 — a non-idempotent create must not retry a mutating 429 into a double-create", posts)
	}
}

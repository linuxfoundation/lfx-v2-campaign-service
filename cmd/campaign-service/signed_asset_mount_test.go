// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"

	goahttp "goa.design/goa/v3/http"
)

const (
	saProject = "cncf"
	saBrief   = "11111111-2222-4333-8444-555555555555"
	saAsset   = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	saOther   = "66666666-7777-4888-9999-aaaaaaaaaaab"
)

var saPNG = []byte("\x89PNG\r\n\x1a\nfake-png-bytes")

// fakeAssetStore holds assets keyed by (project, brief, asset), so a lookup under the wrong
// brief or project misses exactly as the scoped Postgres query does.
type fakeAssetStore struct {
	assets map[[3]string]*model.CreativeAsset
	calls  atomic.Int32
}

func (f *fakeAssetStore) get(p, b, a string) (*model.CreativeAsset, error) {
	f.calls.Add(1)
	if x, ok := f.assets[[3]string{p, b, a}]; ok {
		return x, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeAssetStore) GetAsset(_ context.Context, p, b, a string) (*model.CreativeAsset, error) {
	return f.get(p, b, a)
}

func (f *fakeAssetStore) GetAssetSize(_ context.Context, p, b, a string) (int64, error) {
	x, err := f.get(p, b, a)
	if err != nil {
		return 0, err
	}
	return int64(len(x.Bytes)), nil
}

// countingBudget records reservations so a test can prove the bytes were charged AND
// released.
type countingBudget struct {
	refuse   bool
	held     atomic.Int64
	reserved atomic.Int32
}

func (b *countingBudget) Reserve(_ context.Context, n int64) (func(), bool) {
	if b.refuse {
		return func() {}, false
	}
	b.reserved.Add(1)
	b.held.Add(n)
	return func() { b.held.Add(-n) }, true
}

type saFixture struct {
	srv    *httptest.Server
	signer *assetsign.Signer
	store  *fakeAssetStore
	budget *countingBudget
}

var saNow = time.Unix(1_800_000_000, 0)

func newSignedAssetFixture(t *testing.T, enabled bool, mutate func(*fakeAssetStore, *countingBudget)) *saFixture {
	t.Helper()
	var signer *assetsign.Signer
	if enabled {
		key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
		s, err := assetsign.New(key, "https://lfx-api.example.org", "")
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		signer = s.WithClock(func() time.Time { return saNow })
	}
	store := &fakeAssetStore{assets: map[[3]string]*model.CreativeAsset{
		{saProject, saBrief, saAsset}: {ID: saAsset, ProjectID: saProject, BriefID: saBrief, MimeType: model.MimeTypePNG, Bytes: saPNG},
	}}
	budget := &countingBudget{}
	if mutate != nil {
		mutate(store, budget)
	}
	svc := service.NewSignedAssetService(signer, budget)
	svc.SetCreativeAssetRepo(store)
	mux := goahttp.NewMuxer()
	if err := mountSignedAssets(mux, svc, nil); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &saFixture{srv: srv, signer: signer, store: store, budget: budget}
}

// localURL re-points a minted URL at the test server, keeping path and query exactly.
func (f *saFixture) localURL(t *testing.T, minted string) string {
	t.Helper()
	u, err := url.Parse(minted)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.srv.URL + u.RequestURI()
}

func (f *saFixture) mint(t *testing.T, project, brief, asset string) string {
	t.Helper()
	raw, _, err := f.signer.Mint(project, brief, asset)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return f.localURL(t, raw)
}

func do(t *testing.T, method, target string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestSignedAssetServesVerifiedBytesWithSafeHeaders(t *testing.T) {
	f := newSignedAssetFixture(t, true, nil)
	resp, body := do(t, http.MethodGet, f.mint(t, saProject, saBrief, saAsset))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != string(saPNG) {
		t.Fatalf("body = %q, want the stored bytes", body)
	}
	want := map[string]string{
		"Content-Type":           "image/png",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    "inline",
		"Cache-Control":          "no-store",
	}
	want["Content-Length"] = strconv.Itoa(len(saPNG))
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if f.budget.reserved.Load() != 1 {
		t.Fatalf("asset bytes were reserved %d times, want 1", f.budget.reserved.Load())
	}
	waitReleased(t, f.budget)
}

func TestSignedAssetHEAD(t *testing.T) {
	f := newSignedAssetFixture(t, true, nil)
	resp, body := do(t, http.MethodHead, f.mint(t, saProject, saBrief, saAsset))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD returned a body of %d bytes", len(body))
	}
	if resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Content-Length") != strconv.Itoa(len(saPNG)) {
		t.Fatalf("HEAD headers = %v", resp.Header)
	}
	waitReleased(t, f.budget)
}

// TestSignedAssetRejectionsAreOneGeneric404 drives every rejection cause and asserts they are
// INDISTINGUISHABLE: same status, same headers, same (empty) body. It also proves the store is
// never touched for a request whose signature did not verify.
func TestSignedAssetRejectionsAreOneGeneric404(t *testing.T) {
	f := newSignedAssetFixture(t, true, func(s *fakeAssetStore, _ *countingBudget) {
		// A signable asset whose stored type is outside the allow-list.
		s.assets[[3]string{saProject, saBrief, saOther}] = &model.CreativeAsset{MimeType: "image/gif", Bytes: []byte("GIF89a")}
	})
	good := f.mint(t, saProject, saBrief, saAsset)
	u, _ := url.Parse(good)
	q := u.Query()
	path := u.Path
	sig, exp := q.Get("sig"), q.Get("exp")
	withQuery := func(p, rawQuery string) string { return f.srv.URL + p + "?" + rawQuery }
	flipped := []byte(sig)
	if flipped[5] == 'A' {
		flipped[5] = 'B'
	} else {
		flipped[5] = 'A'
	}
	expiredSigner := f.signer.WithClock(func() time.Time { return saNow.Add(-2 * time.Hour) })
	expiredRaw, _, _ := expiredSigner.Mint(saProject, saBrief, saAsset)

	// Rejections whose signature does NOT verify: the store must not be read.
	unsigned := map[string]string{
		"bad signature":         withQuery(path, "exp="+exp+"&sig="+string(flipped)),
		"expired":               f.localURL(t, expiredRaw),
		"tampered asset id":     withQuery(strings.Replace(path, saAsset, saOther, 1), u.RawQuery),
		"tampered brief id":     withQuery(strings.Replace(path, saBrief, "11111111-2222-4333-8444-555555555556", 1), u.RawQuery),
		"tampered project":      withQuery(strings.Replace(path, "/"+saProject+"/", "/lfn/", 1), u.RawQuery),
		"tampered exp":          withQuery(path, "exp="+strconv.FormatInt(mustInt64(t, exp)-1, 10)+"&sig="+sig),
		"malformed asset id":    withQuery(strings.Replace(path, saAsset, "not-a-uuid", 1), u.RawQuery),
		"traversal project":     withQuery(strings.Replace(path, "/"+saProject+"/", "/..%2f..%2fetc/", 1), u.RawQuery),
		"missing exp":           withQuery(path, "sig="+sig),
		"non-numeric exp":       withQuery(path, "exp=soon&sig="+sig),
		"missing sig":           withQuery(path, "exp="+exp),
		"short sig":             withQuery(path, "exp="+exp+"&sig="+sig[:20]),
		"uppercase asset spell": withQuery(strings.Replace(path, saAsset, strings.ToUpper(saAsset), 1), u.RawQuery),
	}
	// Rejections whose signature DOES verify but the asset fails the serve-time re-check.
	signed := map[string]string{
		"asset gone":          f.mint(t, saProject, saBrief, "77777777-7777-4888-9999-aaaaaaaaaaaa"),
		"foreign brief":       f.mint(t, saProject, "99999999-2222-4333-8444-555555555555", saAsset),
		"foreign project":     f.mint(t, "lfn", saBrief, saAsset),
		"disallowed mimetype": f.mint(t, saProject, saBrief, saOther),
	}

	var ref *http.Response
	check := func(name, target string) {
		resp, body := do(t, http.MethodGet, target)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, resp.StatusCode)
			return
		}
		if len(body) != 0 {
			t.Errorf("%s: 404 carries a body %q", name, body)
		}
		if ref == nil {
			ref = resp
			return
		}
		if a, b := headerShape(ref.Header), headerShape(resp.Header); a != b {
			t.Errorf("%s: 404 headers differ from the reference:\n got  %s\n want %s", name, b, a)
		}
	}
	for name, target := range unsigned {
		check(name, target)
	}
	if n := f.store.calls.Load(); n != 0 {
		t.Fatalf("the asset store was read %d times for requests whose signature did not verify", n)
	}
	for name, target := range signed {
		check(name, target)
	}
	if f.store.calls.Load() == 0 {
		t.Fatal("serve-time ownership re-check never consulted the store")
	}
	waitReleased(t, f.budget)
}

func TestSignedAssetFeatureOffIs404(t *testing.T) {
	on := newSignedAssetFixture(t, true, nil)
	minted := on.mint(t, saProject, saBrief, saAsset)
	u, _ := url.Parse(minted)

	off := newSignedAssetFixture(t, false, nil)
	resp, body := do(t, http.MethodGet, off.srv.URL+u.RequestURI())
	if resp.StatusCode != http.StatusNotFound || len(body) != 0 {
		t.Fatalf("feature off: status %d body %q, want bare 404", resp.StatusCode, body)
	}
	if off.store.calls.Load() != 0 {
		t.Fatal("feature off must not read the store")
	}
}

func TestSignedAssetBudgetExhaustedIs503AfterVerify(t *testing.T) {
	f := newSignedAssetFixture(t, true, func(_ *fakeAssetStore, b *countingBudget) { b.refuse = true })
	resp, body := do(t, http.MethodGet, f.mint(t, saProject, saBrief, saAsset))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("503 carries a body %q", body)
	}
}

func TestMountSignedAssetsRefusesNilService(t *testing.T) {
	if err := mountSignedAssets(goahttp.NewMuxer(), nil, nil); err == nil {
		t.Fatal("mounting a nil service must fail")
	}
}

// headerShape renders a response's headers minus Date, which differs per request.
func headerShape(h http.Header) string {
	var parts []string
	for k, v := range h {
		if k == "Date" {
			continue
		}
		parts = append(parts, k+"="+strings.Join(v, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func waitReleased(t *testing.T, b *countingBudget) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for b.held.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("asset reservation not released: %d bytes still held", b.held.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

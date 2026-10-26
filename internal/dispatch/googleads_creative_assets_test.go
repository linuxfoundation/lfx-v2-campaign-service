// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

const (
	gaAssetID = "11111111-1111-4111-8111-111111111111"
	// The SAME uuid in a second valid spelling. uuid.Parse accepts four, so a caller can name
	// one asset two ways without meaning two assets.
	gaAssetIDBraced = "{11111111-1111-4111-8111-111111111111}"
)

// isPreflightRefusal says the failure happened before anything upstream could have been
// created. It is the distinction that matters for every refusal below: a campaign refused here
// leaves no orphaned budget behind it.
func isPreflightRefusal(err error) bool {
	_, ok := domain.AsPreflightRefusal(err)
	return ok
}

func gaBrief() *model.CampaignBrief {
	return &model.CampaignBrief{ProjectID: "proj-1", ID: "brief-1"}
}

// gaDispatcher builds a dispatcher with only the creative-asset collaborators bound — the
// resolver needs nothing else, and constructing it this way keeps the test off the credential
// and HTTP paths entirely.
func gaDispatcher(assets creativeAssetReader, budget *AssetReserver) *GoogleAdsDispatcher {
	d := &GoogleAdsDispatcher{}
	if assets != nil {
		d.SetCreativeAssetRepo(assets)
	}
	if budget != nil {
		d.SetAssetReserver(budget)
	}
	return d
}

func gaConfigWithRefs(refs ...googleAdsImageRef) *googleAdsConfig {
	return &googleAdsConfig{DemandGenCreative: &googleAdsDemandGenCreativeConfig{
		MarketingImages: googleAdsImageRefs(refs),
	}}
}

func gaStoredAsset(b []byte) *fakeCreativeAssets {
	return &fakeCreativeAssets{asset: &model.CreativeAsset{Bytes: b}}
}

// The happy path, and the contract the platform side depends on: the bytes come back keyed by
// the EXACT string slotEntries will render, so the client's map lookup hits.
func TestResolveGoogleCreativeAssets_KeysBytesBySlotEntry(t *testing.T) {
	store := gaStoredAsset([]byte("image-bytes"))
	cfg := gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID})
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))

	got, release, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer release()

	entry := cfg.DemandGenCreative.MarketingImages.slotEntries()[0]
	if _, ok := got[entry]; !ok {
		t.Fatalf("the resolved map is keyed %v, but the slot renders %q; the client would fetch the token over HTTP", keysOf(got), entry)
	}
	if string(got[entry]) != "image-bytes" {
		t.Errorf("got %q, want the stored bytes", got[entry])
	}
	if n := store.callCount(); n != 1 {
		t.Errorf("the asset was read %d times, want 1", n)
	}
}

// A config naming one asset through two valid uuid spellings must resolve to ONE key, be read
// ONCE, and be charged once. Keying on the caller's raw spelling would read, buffer and budget
// the same image twice, and render two platform entries for one image — which the duplicate
// check would then refuse as "listed more than once".
func TestResolveGoogleCreativeAssets_CanonicalizesAndDedupes(t *testing.T) {
	store := gaStoredAsset([]byte("image-bytes"))
	cfg := gaConfigWithRefs(
		googleAdsImageRef{ImageAssetID: gaAssetIDBraced},
		googleAdsImageRef{ImageAssetID: gaAssetID},
	)
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))

	got, release, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer release()

	if len(got) != 1 {
		t.Errorf("two spellings of one asset resolved to %d keys, want 1: %v", len(got), keysOf(got))
	}
	if n := store.callCount(); n != 1 {
		t.Errorf("the asset was read %d times, want 1", n)
	}
	// The CANONICAL spelling is written back into the config, so the entries the mappers go on
	// to render match the map keys above.
	entries := cfg.DemandGenCreative.MarketingImages.slotEntries()
	if entries[0] != entries[1] {
		t.Errorf("the two entries still render differently: %q and %q", entries[0], entries[1])
	}
	if !strings.HasSuffix(entries[0], gaAssetID) {
		t.Errorf("entry %q does not carry the canonical uuid", entries[0])
	}
}

// Reserve BEFORE the read, never after. Charging afterwards bounds nothing: every concurrent
// dispatch would already hold its full allowance by the time it blocked on the semaphore. A
// budget of zero therefore has to refuse without the asset ever being loaded.
func TestResolveGoogleCreativeAssets_ReservesBeforeReading(t *testing.T) {
	store := gaStoredAsset([]byte("image-bytes"))
	cfg := gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID})
	d := gaDispatcher(store, NewAssetReserver(1, 10*time.Millisecond))

	_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), cfg)
	if err == nil {
		t.Fatal("a budget smaller than the asset was accepted")
	}
	if !isPreflightRefusal(err) {
		t.Errorf("err = %v, want a preflight refusal so nothing upstream is created", err)
	}
	if n := store.callCount(); n != 0 {
		t.Errorf("GetAsset was called %d times; the bytes were materialised before the reservation was taken", n)
	}
}

// A stored row with no bytes cannot become an ImageAsset. Refused HERE, where the message can
// name the asset — passed through, it would reach the platform client as an image it must fetch
// and cannot, and that message would blame a URL that does not exist.
func TestResolveGoogleCreativeAssets_RefusesZeroStoredBytes(t *testing.T) {
	store := gaStoredAsset(nil)
	cfg := gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID})
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))

	_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), cfg)
	if err == nil {
		t.Fatal("an asset with no stored bytes was accepted")
	}
	if !isPreflightRefusal(err) {
		t.Errorf("err = %v, want a preflight refusal", err)
	}
	if !strings.Contains(err.Error(), gaAssetID) {
		t.Errorf("the refusal does not name the asset: %v", err)
	}
}

// Exactly one source per entry. Both sources set is ambiguous and neither is empty, and both are
// refused at RESOLVE time rather than at decode time so the message can name the slot and the
// position the caller actually wrote.
func TestResolveGoogleCreativeAssets_RefusesBothSourcesAndNeither(t *testing.T) {
	for name, ref := range map[string]googleAdsImageRef{
		"both":    {URL: "https://cdn.example.org/a.png", ImageAssetID: gaAssetID},
		"neither": {},
	} {
		t.Run(name, func(t *testing.T) {
			d := gaDispatcher(gaStoredAsset([]byte("x")), NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))
			_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(ref))
			if err == nil {
				t.Fatal("accepted")
			}
			if !isPreflightRefusal(err) {
				t.Errorf("err = %v, want a preflight refusal", err)
			}
			if !strings.Contains(err.Error(), "marketingImages") {
				t.Errorf("the refusal does not name the slot: %v", err)
			}
		})
	}
}

// The slot-entry scheme is reserved against caller input HERE, which is the only place that can
// still tell a caller's string from one the resolver wrote. Left through, slotEntries renders it
// unchanged and the platform client — which tests the prefix before the https rule — would take
// it for a resolved entry rather than a URL to fetch.
//
// The refusal must not echo it: a creative URL can carry a signed query that is the credential
// for the image, and these messages persist unencrypted as Steps entries.
func TestResolveGoogleCreativeAssets_RefusesACallerURLUsingTheReservedScheme(t *testing.T) {
	const secret = "SECRETSIGNATURE"
	store := gaStoredAsset([]byte("x"))
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))
	ref := googleAdsImageRef{URL: googleAdsImageRefScheme + "https://cdn.example.org/a.png?X-Amz-Signature=" + secret}

	_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(ref))
	if err == nil || !isPreflightRefusal(err) {
		t.Fatalf("err = %v, want a preflight refusal", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal echoed the caller's signing query: %v", err)
	}
	if !strings.Contains(err.Error(), "marketingImages") {
		t.Errorf("the refusal does not name the slot: %v", err)
	}
	if n := store.callCount(); n != 0 {
		t.Errorf("the store was consulted %d times for an entry that names no asset", n)
	}
}

// An asset id that is not a uuid is the caller's error, refused before any lookup: it is not a
// key the store could have.
func TestResolveGoogleCreativeAssets_RefusesAMalformedAssetID(t *testing.T) {
	store := gaStoredAsset([]byte("x"))
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))
	_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(googleAdsImageRef{ImageAssetID: "not-a-uuid"}))
	if err == nil || !isPreflightRefusal(err) {
		t.Fatalf("err = %v, want a preflight refusal", err)
	}
	if n := store.callCount(); n != 0 {
		t.Errorf("the store was consulted %d times for an id it could not hold", n)
	}
}

// An asset the brief does not own is refused as not existing, not reported as an internal
// error: GetAssetSize is already scoped to the project and brief, so a missing row IS the
// caller naming something that is not theirs.
func TestResolveGoogleCreativeAssets_RefusesAnAssetThatDoesNotExist(t *testing.T) {
	store := &fakeCreativeAssets{err: domain.ErrNotFound}
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))
	_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID}))
	if err == nil || !isPreflightRefusal(err) {
		t.Fatalf("err = %v, want a preflight refusal", err)
	}
}

// With no store bound, an asset-backed entry is refused cleanly BEFORE spend rather than
// nil-panicking mid-dispatch. The no-database paths construct dispatchers with no store at all,
// so this is reachable, not hypothetical.
func TestResolveGoogleCreativeAssets_RefusesWhenTheStoreIsNotConfigured(t *testing.T) {
	d := gaDispatcher(nil, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))
	_, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID}))
	if err == nil || !isPreflightRefusal(err) {
		t.Fatalf("err = %v, want a preflight refusal", err)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// A URL-only config resolves nothing, reserves nothing and returns a NIL map — so a dispatch
// that names no asset never queues behind one that does, and the platform side's "was anything
// pre-resolved" question has one answer rather than two.
func TestResolveGoogleCreativeAssets_URLOnlyConfigResolvesNothing(t *testing.T) {
	store := gaStoredAsset([]byte("x"))
	d := gaDispatcher(store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))
	got, release, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(),
		gaConfigWithRefs(googleAdsImageRef{URL: "https://cdn.example.org/a.png"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer release()
	if got != nil {
		t.Errorf("got %v, want a nil map", got)
	}
	if n := store.callCount(); n != 0 {
		t.Errorf("the store was consulted %d times for a config naming no asset", n)
	}
}

// A config with no creative at all enumerates no slots, and must return a usable releaser
// rather than a nil func the caller would panic on when it defers it.
func TestResolveGoogleCreativeAssets_NoCreativeReturnsAUsableReleaser(t *testing.T) {
	d := gaDispatcher(nil, nil)
	got, release, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), &googleAdsConfig{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want a nil map", got)
	}
	if release == nil {
		t.Fatal("the releaser is nil; the caller's defer would panic")
	}
	release()
}

// Every reservation taken before a failure is released, or a refused dispatch would hold budget
// against every later one for the life of the process. Proven by draining the budget twice: the
// second resolve can only succeed if the first one's reservation came back.
func TestResolveGoogleCreativeAssets_ReleasesOnFailure(t *testing.T) {
	const size = 16
	budget := NewAssetReserver(size, 10*time.Millisecond)

	// Fails AFTER the reservation is taken: the size is priced and reserved, then the read
	// returns empty bytes, which is refused.
	d := gaDispatcher(&sizedAssets{size: size, bytes: nil}, budget)
	if _, _, err := d.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID})); err == nil {
		t.Fatal("premise: the zero-byte asset must be refused")
	}

	// The whole budget must be available again.
	ok := gaDispatcher(&sizedAssets{size: size, bytes: []byte("0123456789abcdef")}, budget)
	if _, release, err := ok.resolveGoogleCreativeAssets(context.Background(), gaBrief(), gaConfigWithRefs(googleAdsImageRef{ImageAssetID: gaAssetID})); err != nil {
		t.Errorf("the refused dispatch did not release its reservation: %v", err)
	} else {
		release()
	}
}

// sizedAssets prices an asset independently of the bytes it serves, so a test can drive the
// reserve-then-read ordering without a byte slice of the budget's exact length.
type sizedAssets struct {
	size  int64
	bytes []byte
}

func (s *sizedAssets) GetAssetSize(context.Context, string, string, string) (int64, error) {
	return s.size, nil
}

func (s *sizedAssets) GetAsset(context.Context, string, string, string) (*model.CreativeAsset, error) {
	return &model.CreativeAsset{Bytes: s.bytes}, nil
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

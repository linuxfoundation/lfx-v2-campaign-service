// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// compressiblePNG returns a PNG that is TINY on the wire and LARGE decoded.
//
// This is the input the whole aggregate decode bound exists for, and it is built rather than
// asserted about: a uniform image compresses to a few tens of KiB at dimensions whose pixel
// buffer is tens of MiB. A fixture that merely declared a size would prove nothing, because the
// property under test is precisely that wire size does not predict decoded size.
func compressiblePNG(t *testing.T, side int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, side, side))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestCompressiblePNG_AmplifiesFarBeyondItsWireSize pins the premise the bound rests on. If a
// change to the fixture ever made these images incompressible, the tests below would still pass
// while testing nothing, so the amplification itself is asserted.
func TestCompressiblePNG_AmplifiesFarBeyondItsWireSize(t *testing.T) {
	b := compressiblePNG(t, 4000)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	decoded := decodedBytesFor(cfg.Width, cfg.Height, cfg.ColorModel)
	wire := int64(len(b))

	// The wire-priced permit this image would receive upstream, computed from the SHIPPED
	// pricing rule rather than restated.
	upstream := constants.UploadAdmissionWeightFor(wire)
	if decoded <= upstream {
		t.Fatalf("decoded %d <= wire-priced permit %d: this image does not exhibit the "+
			"amplification the decode bound exists to catch, so the tests below prove nothing",
			decoded, upstream)
	}
	t.Logf("wire=%d B (permit %d B) decoded=%d B (%dx amplification)",
		wire, upstream, decoded, decoded/wire)
}

// TestDecodeReserver_BoundsAggregateDecodeMemory is the regression guard for the defect that
// wire-proportional upload pricing introduced.
//
// Pricing upload permits from Content-Length fixed a real availability bug, and created a memory
// one: compression means a few-hundred-KiB body can allocate tens of MiB of pixels, so uploads
// priced at the floor could decode concurrently far past the pod's limit while the upload budget
// still read as unspent.
//
// The assertion is on ADMITTED CONCURRENCY against a known budget, not on wall-clock behaviour:
// N decodes are parked simultaneously inside the reservation and the (N+1)th must be refused.
func TestDecodeReserver_BoundsAggregateDecodeMemory(t *testing.T) {
	const budget = 100
	d := NewDecodeReserver(budget)

	rel1, ok := d.reserve(context.Background(), 60)
	if !ok {
		t.Fatal("first reservation of 60 against a 100 budget was refused")
	}
	// 60 + 60 > 100: the second must be refused while the first is held.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // do not block the test; a refusal is the expected outcome
	if _, ok := d.reserve(ctx, 60); ok {
		t.Error("two 60-byte decodes were admitted against a 100-byte budget: the aggregate " +
			"bound does not bind")
	}
	// A smaller one still fits alongside.
	rel2, ok := d.reserve(context.Background(), 40)
	if !ok {
		t.Error("40 was refused alongside 60 against a 100 budget: the bound is not " +
			"proportional, it is serialising")
	}
	rel2()
	rel1()

	// Once released, the large one fits again.
	rel3, ok := d.reserve(context.Background(), 100)
	if !ok {
		t.Error("a full-budget reservation was refused after every permit was released: " +
			"weight is leaking on release")
	}
	rel3()
}

// TestDecodeReserver_RefusesWhatCouldNeverFit: a single image priced above the whole budget can
// never be admitted, so it must be refused immediately rather than blocking until its context
// expires and then answering the same way.
func TestDecodeReserver_RefusesWhatCouldNeverFit(t *testing.T) {
	d := NewDecodeReserver(100)
	if _, ok := d.reserve(context.Background(), 101); ok {
		t.Error("a reservation larger than the entire budget was admitted")
	}
}

// TestDecodeReserver_NilReservesNothing: every construction that does not wire a reserver must
// behave exactly as before, or this bound changes unrelated call sites.
func TestDecodeReserver_NilReservesNothing(t *testing.T) {
	var d *DecodeReserver
	release, ok := d.reserve(context.Background(), 1<<40)
	if !ok {
		t.Fatal("a nil reserver refused a reservation; it must reserve nothing")
	}
	release() // must not panic
}

// TestUploadCreativeAsset_CompressedImagesCannotExhaustTheDecodeBudget is the END-TO-END form,
// through the real handler, using the exact input the reviewers described: images that are small
// on the wire and large decoded.
//
// It fails without the reservation. With the decode budget set to one image's cost, a second
// concurrent upload of the same image must be shed rather than allowed to allocate a second
// pixel buffer — which is what wire-priced admission alone would have permitted.
func TestUploadCreativeAsset_CompressedImagesCannotExhaustTheDecodeBudget(t *testing.T) {
	const side = 2000
	img := compressiblePNG(t, side)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	one := decodedBytesFor(cfg.Width, cfg.Height, cfg.ColorModel)

	// A budget that fits exactly ONE of these decodes.
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(&blockingCreativeAssetRepo{entered: make(chan struct{}), release: make(chan struct{})})
	s.SetDecodeReserver(NewDecodeReserver(one))

	repo := s.creativeAssetRepo().(*blockingCreativeAssetRepo)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
			ProjectID: "cncf", BriefID: "b1", ContentType: "image/png", Bytes: img,
		})
	}()

	// Wait until the first upload is PAST the decode and holding its reservation.
	<-repo.entered

	// The second must be shed: its pixel buffer does not fit alongside the first.
	//
	// Run on its own goroutine with a bounded wait rather than called inline. Without the
	// reservation the second upload does NOT return — it decodes and blocks in the parked
	// repo — so an inline call would hang the whole package instead of failing this test. A
	// mutation must die with a message, not with a timeout somebody has to diagnose.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	type res struct {
		out *briefs.CreativeAsset
		err error
	}
	second := make(chan res, 1)
	go func() {
		o, e := s.UploadCreativeAsset(ctx, &briefs.UploadCreativeAssetPayload{
			ProjectID: "cncf", BriefID: "b1", ContentType: "image/png", Bytes: img,
		})
		second <- res{o, e}
	}()
	select {
	case r := <-second:
		err = r.err
	case <-time.After(15 * time.Second):
		close(repo.release)
		wg.Wait()
		t.Fatalf("the second upload neither returned nor was shed: it was admitted past the "+
			"decode reservation and blocked in the handler, so a %d-byte decode budget did not "+
			"stop a second %d-byte pixel buffer", one, one)
	}
	if err == nil {
		t.Errorf("a second %d-byte upload decoding to %d bytes was admitted against a %d-byte "+
			"decode budget; wire-priced admission alone cannot stop this, so concurrent "+
			"compressed uploads can exhaust the pod", len(img), one, one)
	}
	var unavailable *briefs.ConnServiceUnavailableError
	if err != nil && !errors.As(err, &unavailable) {
		t.Errorf("shed error = %T (%v), want *briefs.ConnServiceUnavailableError (retryable 503)", err, err)
	}

	close(repo.release)
	wg.Wait()
}

// blockingCreativeAssetRepo parks inside CreateAsset, which runs AFTER the decode and while the
// decode reservation is still held, so a test can observe the reservation as occupied.
type blockingCreativeAssetRepo struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func (f *blockingCreativeAssetRepo) CreateAsset(_ context.Context, a *model.CreativeAsset) (*model.CreativeAsset, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.entered) })
	<-f.release
	out := *a
	out.ID = "asset-1"
	return &out, nil
}

func (f *blockingCreativeAssetRepo) GetAsset(_ context.Context, _, _, _ string) (*model.CreativeAsset, error) {
	return nil, domain.ErrNotFound
}

// TestDecodedBytesFor_MatchesTheGateItShares: the reservation and the per-image gate must charge
// the same arithmetic, or the aggregate bound under-counts what the gate admits.
func TestDecodedBytesFor_MatchesTheGateItShares(t *testing.T) {
	cases := []struct {
		w, h int
		m    color.Model
	}{
		{100, 100, color.RGBAModel},
		{4000, 4000, color.RGBAModel},
		{1, 1, color.NRGBA64Model},
		{5000, 4000, color.NRGBA64Model},
	}
	for _, c := range cases {
		got := decodedBytesFor(c.w, c.h, c.m)
		want := int64(c.w) * int64(c.h) * bytesPerPixelFor(c.m)
		if got != want {
			t.Errorf("decodedBytesFor(%d,%d) = %d, want %d", c.w, c.h, got, want)
		}
		// And the gate must agree about admissibility at that figure.
		if allowed := dimensionsWithinLimits(c.w, c.h, c.m); allowed != (got <= maxCreativeDecodedBytes) {
			t.Errorf("gate and cost disagree for %dx%d: allowed=%v cost=%d budget=%d",
				c.w, c.h, allowed, got, int64(maxCreativeDecodedBytes))
		}
	}
}

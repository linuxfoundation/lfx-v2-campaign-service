// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

// resolvedRef renders a slot entry the way internal/dispatch does, from the same exported
// constant, so a drift in the prefix fails these tests rather than silently turning every
// pre-resolved image back into an HTTP fetch of a string that is not a URL.
func resolvedRef(id string) string { return ResolvedImageRefPrefix + id }

// A pre-resolved entry is a CALLER invariant: the code that wrote the token is the code that
// fills the map. If the bytes are missing the entry must be refused in the shape phase, where
// every other preflight refusal lands — before the budget, campaign and ad group exist. Passed
// through, it would reach the fetch as an image to download from "lfx-asset:<uuid>".
func TestValidateImageEntries_ResolvedToken(t *testing.T) {
	slot := demandGenImageSlots[0]
	const id = "11111111-1111-4111-8111-111111111111"
	ref := resolvedRef(id)

	t.Run("no bytes supplied is refused", func(t *testing.T) {
		_, err := validateImageEntries(slot, []string{ref}, map[string][]byte{})
		if err == nil {
			t.Fatal("a token with no bytes behind it was accepted")
		}
		if !strings.Contains(err.Error(), "no resolved image bytes") {
			t.Errorf("expected the missing-bytes refusal, got: %v", err)
		}
		// The id is this service's own primary key, not a credential, and a refusal that
		// named nothing would leave an operator with no way to find the asset.
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the refusal does not name the asset: %v", err)
		}
	})

	t.Run("a nil map is the same refusal", func(t *testing.T) {
		// The nil map is the live shape for a config naming no asset at all —
		// resolveGoogleCreativeAssets returns nil rather than an empty map — so a token
		// reaching this path with nil must not read as "supplied".
		if _, err := validateImageEntries(slot, []string{ref}, nil); err == nil {
			t.Fatal("a token was accepted against a nil resolved map")
		}
	})

	t.Run("bytes supplied passes through byte for byte", func(t *testing.T) {
		got, err := validateImageEntries(slot, []string{ref}, map[string][]byte{ref: []byte("x")})
		if err != nil {
			t.Fatalf("a token whose bytes were supplied must be accepted: %v", err)
		}
		// The entry is the MAP KEY: the fetch phase looks it up again, so rewriting it
		// here would turn a resolved image into an HTTP request for the token.
		if len(got) != 1 || got[0] != ref {
			t.Errorf("got %q, want the entry unchanged as %q", got, ref)
		}
	})

	t.Run("the duplicate rule applies to tokens too", func(t *testing.T) {
		_, err := validateImageEntries(slot, []string{ref, ref}, map[string][]byte{ref: []byte("x")})
		if err == nil {
			t.Fatal("the same asset listed twice was accepted")
		}
		if !strings.Contains(err.Error(), "more than once") {
			t.Errorf("expected the duplicate refusal, got: %v", err)
		}
	})
}

// The prefix branch is tested BEFORE the https rule, so any string carrying the prefix lands
// there — including one a caller wrote into the config. Only the canonical uuid shape the
// dispatch resolver renders may be treated as a resolved entry, and anything else must be
// refused without being echoed: all that is known about it is its prefix, and a creative URL's
// query string can be the credential for the image.
func TestValidateImageEntries_ResolvedTokenWithAMalformedSuffix(t *testing.T) {
	slot := demandGenImageSlots[0]
	const secret = "SECRETSIGNATURE"

	for name, suffix := range map[string]string{
		"a caller URL":   "https://cdn.example.org/a.png?X-Amz-Signature=" + secret,
		"not a uuid":     "not-a-uuid",
		"empty":          "",
		"uppercase uuid": "11111111-1111-4111-8111-11111111111A",
	} {
		t.Run(name, func(t *testing.T) {
			entry := ResolvedImageRefPrefix + suffix
			// Supplied in the map too, so the refusal is the shape check firing rather
			// than the missing-bytes rule.
			_, err := validateImageEntries(slot, []string{entry}, map[string][]byte{entry: []byte("x")})
			if err == nil {
				t.Fatal("a malformed resolved entry was accepted")
			}
			if strings.Contains(err.Error(), suffix) && suffix != "" {
				t.Errorf("the refusal echoed the caller's value: %v", err)
			}
			if !strings.Contains(err.Error(), slot.label) {
				t.Errorf("the refusal does not name the slot: %v", err)
			}
		})
	}
}

// imageRefForError exists because redactURLForError is wrong for a token and wrong SILENTLY:
// "lfx-asset:<uuid>" parses, and is absolute, but has no Host — the one condition that collapses
// the whole value to "(redacted)". A refusal naming an asset would then name nothing.
func TestImageRefForError_RendersATokenButStillRedactsAURL(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	got := imageRefForError(resolvedRef(id))
	if got != resolvedRef(id) {
		t.Errorf("a resolved token rendered as %q, want it verbatim as %q", got, resolvedRef(id))
	}
	if strings.Contains(got, "redacted") {
		t.Errorf("the token collapsed to a placeholder: %q", got)
	}

	// The other half of the same function, so the fix above cannot widen into rendering
	// caller URLs verbatim: a signed query string IS the credential, and these messages
	// persist unencrypted as Steps entries.
	const secret = "SECRETSIGNATURE"
	url := imageRefForError("https://cdn.example.org/a.png?X-Amz-Signature=" + secret)
	if strings.Contains(url, secret) {
		t.Errorf("a caller URL kept its signing query: %q", url)
	}
	if !strings.Contains(url, "cdn.example.org") {
		t.Errorf("a caller URL lost its host entirely: %q", url)
	}
}

// A pre-resolved image runs the SAME byte checks as a fetched one. Skipping them would make an
// asset-backed slot the one way to get an out-of-spec image past preflight and into the ad
// mutate — which runs after the budget and campaign have committed, which is the orphaned-campaign
// state the whole preflight design exists to avoid.
func TestFetchSlotImages_ResolvedImageStillFailsGeometry(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	ref := resolvedRef(id)
	slot := demandGenImageSlots[0] // 1.91:1, minimum 600x314

	c := imageFetchTestClient(t)
	_, err := c.fetchSlotImages(context.Background(), demandGenImageSlots[:1],
		[][]string{{ref}}, map[string][]byte{ref: pngOf(t, 100, 100)})
	if err == nil {
		t.Fatal("a 100x100 pre-resolved image was accepted into a 1.91:1 slot")
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("the refusal does not name the asset: %v", err)
	}
	if !strings.Contains(err.Error(), slot.label) {
		t.Errorf("the refusal does not name the slot: %v", err)
	}

	// The same slot with a conforming image is accepted, so the refusal above is the
	// geometry check firing rather than pre-resolved entries being rejected wholesale.
	got, err := c.fetchSlotImages(context.Background(), demandGenImageSlots[:1],
		[][]string{{ref}}, map[string][]byte{ref: pngOf(t, 1200, 628)})
	if err != nil {
		t.Fatalf("a conforming pre-resolved image must be accepted: %v", err)
	}
	if len(got) != 1 || got[0].url != ref {
		t.Fatalf("got %+v, want one entry carrying %q", got, ref)
	}
	if len(got[0].data) == 0 {
		t.Error("the resolved bytes were not attached to the fetched image")
	}
}

// Resolved bytes count into the creative-wide total exactly as downloaded bytes do. They are the
// case that matters most: they arrive from this service's own store with no HTTP transfer to make
// a large set slow, so a total that only counted fetches would let an all-asset creative hold
// hundreds of megabytes resident — the allocation this cap exists to prevent.
func TestFetchSlotImages_ResolvedBytesCountIntoTheCreativeTotal(t *testing.T) {
	// ONE image, referenced under many keys. The bytes are incompressible noise so the
	// encoded PNG is genuinely large, and sharing the slice keeps the test's own memory
	// to a single image while the running total sees every reference.
	img := noisePNG(t, 1200, 628)
	if len(img) > maxDemandGenImageBytes {
		t.Fatalf("fixture is wrong: %d bytes exceeds the per-image cap of %d", len(img), maxDemandGenImageBytes)
	}
	over := maxCreativeTotalImageBytes/len(img) + 1

	refs := make([]string, 0, over)
	resolved := make(map[string][]byte, over)
	for i := range over {
		ref := resolvedRef(fmt.Sprintf("33333333-3333-4333-8333-%012d", i))
		refs = append(refs, ref)
		resolved[ref] = img
	}

	c := imageFetchTestClient(t)
	_, err := c.fetchSlotImages(context.Background(), demandGenImageSlots[:1], [][]string{refs}, resolved)
	if err == nil {
		t.Fatalf("%d resolved images of %d bytes total more than the %d byte cap and must be refused",
			over, len(img), maxCreativeTotalImageBytes)
	}
	if !strings.Contains(err.Error(), "across all slots") {
		t.Errorf("expected the creative-wide total refusal, got: %v", err)
	}

	// One image short of the sum is accepted, so the refusal above is the total firing and
	// not a per-image or per-slot rule.
	under := refs[:over-1]
	got, uerr := c.fetchSlotImages(context.Background(), demandGenImageSlots[:1], [][]string{under}, resolved)
	if uerr != nil {
		t.Fatalf("%d resolved images stay under the %d byte cap and must be accepted: %v",
			len(under), maxCreativeTotalImageBytes, uerr)
	}
	if len(got) != len(under) {
		t.Errorf("attached %d images, want %d", len(got), len(under))
	}
}

// noisePNG encodes an image whose pixels do not compress, so its encoded size is proportional to
// its dimensions. pngOf's near-uniform image encodes to a few hundred bytes at any size, which
// cannot exercise a byte cap.
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Fixed seed: the fixture's size has to be the same on every run, because the number of
	// references the test builds is derived from it.
	rnd := rand.New(rand.NewSource(1))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

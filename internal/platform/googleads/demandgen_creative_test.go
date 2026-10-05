// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// pngOf renders a real PNG of the given size. The tests decode actual image bytes
// rather than stubbing the decoder, because the dimension and aspect-ratio rules are
// the whole point of fetching the bytes at all — asserting them against a fake would
// assert nothing about Google's contract.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

// imageServer serves one body per path and returns the base URL. Callers reach it
// through a client built with allowLoopbackGuard, since the production guard refuses
// 127.0.0.1 — which is itself asserted separately, in TestCheckPublicIP.
func imageServer(t *testing.T, bodies map[string][]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func allowLoopbackGuard(net.IP) error { return nil }

// imageFetchTestClient is a client whose creative fetch may reach loopback AND whose
// URL scheme check is satisfied, by rewriting the httptest http:// origin. The scheme
// rule is enforced in the pure validator, so these tests exercise the fetch by calling
// fetchOneImage directly rather than going through validateDemandGenCreative.
func imageFetchTestClient(t *testing.T) *Client {
	t.Helper()
	return NewClient(testCreds(), testAccount(), WithClock(fixedClock()), withImageDialGuard(allowLoopbackGuard))
}

// fullCreative is a creative that passes every local rule, for tests that want to
// vary exactly one thing.
func fullCreative() DemandGenCreative {
	return DemandGenCreative{
		MarketingImages: []string{"https://cdn.example.org/m1.png"},
		LogoImages:      []string{"https://cdn.example.org/logo.png"},
		Headlines:       []string{"Join us at KubeCon"},
		Descriptions:    []string{"Three days of talks, workshops and hallway track."},
		BusinessName:    "Linux Foundation",
	}
}

// creativeInput is demandgen_test.go's minimal Demand Gen input with a creative
// attached, so these tests vary only the creative.
func creativeInput(c DemandGenCreative) CampaignInput {
	in := demandGenInput()
	in.DemandGenCreative = c
	return in
}

// ---------------------------------------------------------------------------
// validateDemandGenCreative — the pure half
// ---------------------------------------------------------------------------

// Absent creative must stay valid. Every Demand Gen campaign created before this
// feature existed has no creative, and ValidateCampaignInputKind runs the same
// preflight for adoption — refusing them now would break adoption of rows already
// in the database.
func TestValidateDemandGenCreative_AbsentIsAccepted(t *testing.T) {
	plan, err := validateDemandGenCreative(campaignKindDemandGen, demandGenInput())
	if err != nil {
		t.Fatalf("an empty creative must be accepted: %v", err)
	}
	if plan.present {
		t.Error("an empty creative must not produce a present plan")
	}
}

// The creative is a Demand Gen capability, so it is REFUSED on Search rather than
// accepted and dropped — the same choice every other channel-specific input in this
// preflight makes, and for the same reason: a silent drop makes the operator believe
// a creative is live when no ad carries it.
func TestValidateDemandGenCreative_RefusedOnSearch(t *testing.T) {
	_, err := validateDemandGenCreative(campaignKindSearch, creativeInput(fullCreative()))
	if err == nil {
		t.Fatal("Demand Gen creative must be refused on a Search campaign")
	}
	if !strings.Contains(err.Error(), campaignKindSearch) {
		t.Errorf("the refusal must name the channel it was refused on, got: %v", err)
	}
}

func TestValidateDemandGenCreative_HappyPath(t *testing.T) {
	plan, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(fullCreative()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.present {
		t.Fatal("a supplied creative must produce a present plan")
	}
	if got := plan.imageCount(); got != 2 {
		t.Errorf("imageCount() = %d, want 2", got)
	}
	if plan.businessName != "Linux Foundation" {
		t.Errorf("businessName = %q", plan.businessName)
	}
	// Positional parity with demandGenImageSlots is what pairs each URL with its
	// shape rules and its json key; a plan that lost it would upload the logo as a
	// marketing image.
	if len(plan.urls) != len(demandGenImageSlots) {
		t.Fatalf("plan.urls has %d slots, want %d", len(plan.urls), len(demandGenImageSlots))
	}
	if len(plan.urls[0]) != 1 || len(plan.urls[logoSlotIndex]) != 1 {
		t.Errorf("urls landed in the wrong slots: %v", plan.urls)
	}
}

func TestValidateDemandGenCreative_Rejections(t *testing.T) {
	long := strings.Repeat("a", 200)
	cases := []struct {
		name    string
		mutate  func(*DemandGenCreative)
		wantSub string
	}{
		{"no logo", func(c *DemandGenCreative) { c.LogoImages = nil }, "at least 1 logo"},
		{"too many logos", func(c *DemandGenCreative) {
			c.LogoImages = []string{"https://e.org/1.png", "https://e.org/2.png", "https://e.org/3.png", "https://e.org/4.png", "https://e.org/5.png", "https://e.org/6.png"}
		}, "at most 5 logo"},
		{"neither required marketing shape", func(c *DemandGenCreative) {
			c.MarketingImages = nil
			c.PortraitImages = []string{"https://e.org/p.png"}
		}, "at least one marketing image"},
		{"no headline", func(c *DemandGenCreative) { c.Headlines = nil }, "at least 1 headline"},
		{"six headlines", func(c *DemandGenCreative) {
			c.Headlines = []string{"a", "b", "c", "d", "e", "f"}
		}, "at most 5 headline"},
		{"six descriptions", func(c *DemandGenCreative) {
			c.Descriptions = []string{"a", "b", "c", "d", "e", "f"}
		}, "at most 5 description"},
		{"over-wide headline", func(c *DemandGenCreative) { c.Headlines = []string{long} }, "display width"},
		{"over-wide description", func(c *DemandGenCreative) { c.Descriptions = []string{long} }, "display width"},
		{"duplicate headline", func(c *DemandGenCreative) {
			c.Headlines = []string{"Join us", "JOIN US"}
		}, "more than once"},
		{"no business name", func(c *DemandGenCreative) { c.BusinessName = "  " }, "business name"},
		{"over-wide business name", func(c *DemandGenCreative) { c.BusinessName = long }, "display width"},
		{"over-long call to action", func(c *DemandGenCreative) { c.CallToActionText = long }, "call to action"},
		{"http image URL", func(c *DemandGenCreative) {
			c.MarketingImages = []string{"http://cdn.example.org/m1.png"}
		}, "https"},
		{"unparseable image URL", func(c *DemandGenCreative) {
			c.MarketingImages = []string{"https://exa mple.org/\x7f"}
		}, "marketing image"},
		{"duplicate image URL", func(c *DemandGenCreative) {
			c.MarketingImages = []string{"https://e.org/a.png", "https://e.org/a.png"}
		}, "more than once"},
		{"empty headline", func(c *DemandGenCreative) { c.Headlines = []string{"ok", "  "} }, "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fullCreative()
			tc.mutate(&c)
			_, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c))
			if err == nil {
				t.Fatalf("expected a refusal mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// The 20-image ceiling is Google's own COMBINED total across the four marketing
// arrays. Checked across them rather than per array: four arrays of 19 is 76 images
// and satisfies every per-array reading of the rule.
func TestValidateDemandGenCreative_MarketingCeilingIsCombined(t *testing.T) {
	fill := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, "https://e.org/"+prefix+string(rune('a'+i))+".png")
		}
		return out
	}
	c := fullCreative()
	c.MarketingImages = fill("m", 6)
	c.SquareMarketingImages = fill("s", 6)
	c.PortraitImages = fill("p", 5)
	c.TallPortraitImages = fill("t", 4) // 21 combined, none over 20 alone
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c)); err == nil {
		t.Fatal("21 marketing images across four arrays must be refused")
	}
	c.TallPortraitImages = fill("t", 3) // exactly 20
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c)); err != nil {
		t.Fatalf("exactly 20 marketing images must be accepted: %v", err)
	}
}

// Logos are counted separately from the marketing ceiling. Folding them in would
// refuse a 20-marketing-image ad that also carries the logo Google REQUIRES — an
// over-refusal of a creative Google accepts.
func TestValidateDemandGenCreative_LogosAreOutsideTheMarketingCeiling(t *testing.T) {
	c := fullCreative()
	c.MarketingImages = nil
	c.SquareMarketingImages = make([]string, 0, maxDemandGenMarketingImages)
	for i := 0; i < maxDemandGenMarketingImages; i++ {
		c.SquareMarketingImages = append(c.SquareMarketingImages, "https://e.org/s"+string(rune('a'+i))+".png")
	}
	c.LogoImages = []string{"https://e.org/l1.png", "https://e.org/l2.png"}
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c)); err != nil {
		t.Fatalf("20 marketing images plus 2 logos must be accepted: %v", err)
	}
}

// The counts are Demand Gen's, not the RSA ones. This is the mismatched-contract
// trap the weight helpers invite: the display widths DO coincide (30 / 90), so a
// test that only checked widths would pass with maxHeadlines wired in by mistake.
func TestValidateDemandGenCreative_CountsAreNotTheRSACounts(t *testing.T) {
	if maxDemandGenHeadlines >= maxHeadlines {
		t.Fatalf("test premise broken: Demand Gen allows %d headlines, RSA %d", maxDemandGenHeadlines, maxHeadlines)
	}
	c := fullCreative()
	c.Headlines = []string{"one", "two", "three", "four", "five", "six"} // legal for an RSA
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c)); err == nil {
		t.Error("six headlines is legal for an RSA and must still be refused for Demand Gen")
	}
}

// Over-long copy is REFUSED, not truncated. ad_copy.go truncates because it
// generates its own copy and pads from defaults; this copy is written by a human,
// and shipping it cut mid-word is worse than refusing while nothing is paid for.
func TestValidateDemandGenCreative_CopyIsRefusedNotTruncated(t *testing.T) {
	c := fullCreative()
	c.Headlines = []string{strings.Repeat("x", maxHeadlineWeight+1)}
	plan, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c))
	if err == nil {
		t.Fatalf("over-wide copy must be refused, got plan with headlines %v", plan.headlines)
	}
}

// Double-width characters count double, exactly as they do for an RSA: Google states
// both limits as a "maximum display width", which is the one contract these two
// paths genuinely share.
func TestValidateDemandGenCreative_DisplayWidthIsDoubleWidthAware(t *testing.T) {
	c := fullCreative()
	// 16 CJK runes = 32 display units, over the 30 limit but only 16 runes.
	c.Headlines = []string{strings.Repeat("会", 16)}
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c)); err == nil {
		t.Error("16 double-width runes exceed the 30-unit display width and must be refused")
	}
	c.Headlines = []string{strings.Repeat("会", 15)} // exactly 30
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(c)); err != nil {
		t.Errorf("exactly 30 display units must be accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// preflight integration
// ---------------------------------------------------------------------------

// The creative must be resolved by preflightCampaignKind itself, which is what makes
// ValidateCampaignInputKind refuse exactly what the create cascade refuses.
func TestPreflightCampaignKind_ResolvesTheCreative(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, creativeInput(fullCreative()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pf.creative.present {
		t.Error("the preflight did not resolve the creative")
	}
	if _, err := c.preflightCampaignKind(campaignKindDemandGen, creativeInput(DemandGenCreative{
		MarketingImages: []string{"https://e.org/m.png"},
		BusinessName:    "LF",
		Headlines:       []string{"h"},
		Descriptions:    []string{"d"},
	})); err == nil {
		t.Error("a creative with no logo must fail the preflight, before any mutate")
	}
}

// ---------------------------------------------------------------------------
// checkImageGeometry
// ---------------------------------------------------------------------------

func TestCheckImageGeometry(t *testing.T) {
	marketing := demandGenImageSlots[0] // 1.91:1, min 600x314
	square := demandGenImageSlots[1]    // 1:1, min 300x300
	logo := demandGenImageSlots[logoSlotIndex]

	cases := []struct {
		name    string
		slot    imageSlot
		w, h    int
		wantErr bool
	}{
		{"marketing exactly at the minimum", marketing, 600, 314, false},
		{"marketing below the minimum", marketing, 599, 313, true},
		// 1200/628 = 1.9108, within 1% of 1.91 — the standard 1.91:1 asset size.
		{"marketing at the common 1200x628", marketing, 1200, 628, false},
		{"marketing at 16:9, outside tolerance", marketing, 1600, 900, true},
		{"square exact", square, 300, 300, false},
		{"square slightly off, inside tolerance", square, 301, 300, false},
		{"square well off", square, 400, 300, true},
		{"logo exact", logo, 128, 128, false},
		{"logo too small", logo, 127, 127, true},
		{"zero dimensions", logo, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkImageGeometry(tc.slot, "https://e.org/x.png", "png", tc.w, tc.h)
			if tc.wantErr && err == nil {
				t.Fatalf("%dx%d must be refused for a %s", tc.w, tc.h, tc.slot.label)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%dx%d must be accepted for a %s: %v", tc.w, tc.h, tc.slot.label, err)
			}
		})
	}
}

// The tolerance is symmetric: an image 1% too WIDE and one 1% too NARROW are equally
// off, and a one-sided check would accept half the images Google refuses.
func TestCheckImageGeometry_ToleranceIsSymmetric(t *testing.T) {
	square := demandGenImageSlots[1]
	if err := checkImageGeometry(square, "u", "png", 400, 300); err == nil {
		t.Error("4:3 is too wide for a 1:1 slot and must be refused")
	}
	if err := checkImageGeometry(square, "u", "png", 300, 400); err == nil {
		t.Error("3:4 is too tall for a 1:1 slot and must be refused")
	}
}

// ---------------------------------------------------------------------------
// checkPublicIP
// ---------------------------------------------------------------------------

func TestCheckPublicIP(t *testing.T) {
	cases := []struct {
		ip      string
		wantErr bool
	}{
		{"93.184.216.34", false},
		{"2606:2800:220:1:248:1893:25c8:1946", false},
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"10.0.0.5", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud instance metadata
		{"100.64.0.1", true},      // carrier-grade NAT
		{"100.127.255.255", true},
		{"224.0.0.1", true},
		{"fc00::1", true}, // IPv6 unique local
		{"fe80::1", true}, // IPv6 link local
		// An IPv4-mapped IPv6 address must be judged as the IPv4 address it carries,
		// not waved through because none of the IPv6 predicates match it.
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		// 100.128.0.0 is outside the CGNAT /10 and must NOT be caught by a check that
		// matched on the first octet alone.
		{"100.128.0.1", false},
		{"99.255.255.255", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			err := checkPublicIP(net.ParseIP(tc.ip))
			if tc.wantErr && err == nil {
				t.Fatalf("%s must be refused", tc.ip)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s must be allowed: %v", tc.ip, err)
			}
		})
	}
	if err := checkPublicIP(nil); err == nil {
		t.Error("an unparseable address must be refused")
	}
}

// The default is the REAL guard. A client built as a zero value, or one whose guard
// was never set, must not silently get an unrestricted fetch — that is the one
// default that must fail closed.
func TestImageFetchClient_DefaultsToTheRealGuard(t *testing.T) {
	var c Client
	base := imageServer(t, map[string][]byte{"/m.png": pngOf(t, 600, 314)})
	_, err := c.fetchOneImage(context.Background(), demandGenImageSlots[0], base+"/m.png")
	if err == nil {
		t.Fatal("a client with no explicit guard must refuse a loopback fetch")
	}
}

// ---------------------------------------------------------------------------
// fetchOneImage
// ---------------------------------------------------------------------------

func TestFetchOneImage(t *testing.T) {
	base := imageServer(t, map[string][]byte{
		"/good.png":   pngOf(t, 1200, 628),
		"/good.jpg":   jpegOf(t, 1200, 628),
		"/small.png":  pngOf(t, 100, 52),
		"/square.png": pngOf(t, 700, 700), // large enough; only the ratio is wrong
		"/empty.png":  {},
		"/junk.png":   []byte("this is not an image"),
	})
	c := imageFetchTestClient(t)
	marketing := demandGenImageSlots[0]

	t.Run("png", func(t *testing.T) {
		data, err := c.fetchOneImage(context.Background(), marketing, base+"/good.png")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(data) == 0 {
			t.Error("no bytes returned")
		}
	})
	t.Run("jpeg", func(t *testing.T) {
		if _, err := c.fetchOneImage(context.Background(), marketing, base+"/good.jpg"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("404", func(t *testing.T) {
		_, err := c.fetchOneImage(context.Background(), marketing, base+"/missing.png")
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("a 404 must be reported as such, got: %v", err)
		}
	})
	t.Run("below the minimum size", func(t *testing.T) {
		_, err := c.fetchOneImage(context.Background(), marketing, base+"/small.png")
		if err == nil || !strings.Contains(err.Error(), "minimum") {
			t.Fatalf("an undersized image must be refused, got: %v", err)
		}
	})
	t.Run("wrong aspect ratio", func(t *testing.T) {
		_, err := c.fetchOneImage(context.Background(), marketing, base+"/square.png")
		if err == nil || !strings.Contains(err.Error(), "aspect ratio") {
			t.Fatalf("a 1:1 image must be refused for a 1.91:1 slot, got: %v", err)
		}
	})
	t.Run("empty body", func(t *testing.T) {
		if _, err := c.fetchOneImage(context.Background(), marketing, base+"/empty.png"); err == nil {
			t.Fatal("an empty body must be refused")
		}
	})
	// The decoder, not the file extension or the Content-Type, decides what the
	// bytes are. A .png serving HTML must not be uploaded as an image asset.
	t.Run("not an image", func(t *testing.T) {
		_, err := c.fetchOneImage(context.Background(), marketing, base+"/junk.png")
		if err == nil || !strings.Contains(err.Error(), "not a usable") {
			t.Fatalf("non-image bytes must be refused, got: %v", err)
		}
	})
}

// The cap is enforced by what the client reads, not by what the host declares: a
// host that sends a chunked body with no length at all must still be cut off, and
// the refusal must be the size one rather than a truncated image reaching Google.
func TestFetchOneImage_SizeCapBindsOnAnUndeclaredLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		chunk := bytes.Repeat([]byte{0}, 1<<16)
		for written := 0; written <= maxDemandGenImageBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c := imageFetchTestClient(t)
	_, err := c.fetchOneImage(context.Background(), demandGenImageSlots[0], srv.URL+"/big.png")
	if err == nil {
		t.Fatal("an over-sized body must be refused")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("expected a size refusal, got: %v", err)
	}
}

// Redirects are refused outright rather than followed: following one would re-open
// every destination check against an address the original URL never named.
func TestFetchOneImage_RefusesRedirects(t *testing.T) {
	// Rendered on the TEST goroutine: pngOf ends in t.Fatalf, and FailNow from a handler
	// does not stop the test — it can leave the server blocked while a deferred Close runs.
	body := pngOf(t, 1200, 628)
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect.png" {
			http.Redirect(w, r, target+"/real.png", http.StatusFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	target = srv.URL
	c := imageFetchTestClient(t)
	if _, err := c.fetchOneImage(context.Background(), demandGenImageSlots[0], srv.URL+"/redirect.png"); err == nil {
		t.Fatal("a redirect must be refused, not followed")
	}
}

// The fetch must carry no Google credentials: the destination is an address the
// caller chose, and leaking the account's bearer token or developer token to it
// would be far worse than any image problem.
func TestFetchOneImage_SendsNoCredentials(t *testing.T) {
	body := pngOf(t, 1200, 628)
	// The captured headers ARE the assertion, and they cross from the handler goroutine to
	// this one — so the handoff needs a happens-before edge, not just the fetch returning.
	var (
		mu  sync.Mutex
		got http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := imageFetchTestClient(t)
	if _, err := c.fetchOneImage(context.Background(), demandGenImageSlots[0], srv.URL+"/m.png"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	hdr := got.Clone()
	mu.Unlock()
	for _, h := range []string{"Authorization", "Developer-Token", "Login-Customer-Id"} {
		if v := hdr.Get(h); v != "" {
			t.Errorf("creative fetch leaked %s: %q", h, v)
		}
	}
}

// ---------------------------------------------------------------------------
// the cascade
// ---------------------------------------------------------------------------

// demandGenTLSServer serves BOTH the creative images and the Google Ads API over
// TLS. TLS, not plain http, because the creative URLs have to survive the pure
// validator's https-only rule — exercising the cascade against http:// URLs would
// mean relaxing that rule for the tests, which is the one thing a test must not do
// to the rule it is meant to protect.
func demandGenTLSServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// demandGenClient points a client at a TLS test server, trusting that server's
// certificate for both the API calls and the creative fetch, and allowing the
// creative fetch to reach loopback. The dial guard and the https rule are each
// asserted on their own above; here they would only prevent the cascade from
// running at all.
func demandGenClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(srv.URL), WithClock(fixedClock()),
		WithHTTPClient(srv.Client()),
		withRetryBaseDelay(time.Millisecond),
		withImageDialGuard(allowLoopbackGuard), withImageTLSConfig(tlsCfg))
}

// demandGenCascade routes the four pre-existing Demand Gen mutates to their happy
// handlers and leaves the two creative mutates to the caller, so each test varies
// only the part it is about. Any path that is neither is served as an image.
func demandGenCascade(t *testing.T, images map[string][]byte, assetH, adH http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			assetH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			adH(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// okAssets answers assets:mutate with two asset resource names, and records the
// request body.
func okCreativeAssets() (http.HandlerFunc, func() string) {
	return capturedMutate(2, func(i int) string {
		return "customers/1234567890/assets/" + []string{"900", "901"}[i]
	})
}

func creativeAt(base string) DemandGenCreative {
	return DemandGenCreative{
		MarketingImages:  []string{base + "/m.png"},
		LogoImages:       []string{base + "/logo.png"},
		Headlines:        []string{"Join us at KubeCon"},
		Descriptions:     []string{"Three days of talks."},
		BusinessName:     "Linux Foundation",
		CallToActionText: "Register",
	}
}

// twoImages is the pair creativeAt asks for, at sizes that satisfy both slots.
func twoImages(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"/m.png":    pngOf(t, 1200, 628),
		"/logo.png": pngOf(t, 256, 256),
	}
}

func TestCreateDemandGenCampaign_WithCreative(t *testing.T) {
	assetH, readAsset := okCreativeAssets()
	adH, readAd := capturedMutate(1, func(int) string {
		return "customers/1234567890/adGroupAds/333~444"
	})
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH, adH))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.AdID != "444" {
		t.Errorf("AdID = %q, want 444", res.AdID)
	}
	if len(res.CreativeAssetIDs) != 2 {
		t.Errorf("CreativeAssetIDs = %v, want 2 ids", res.CreativeAssetIDs)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "Demand Gen ad created: 444") {
		t.Errorf("steps do not report the ad:\n%s", joined)
	}
	// The closing step must no longer tell the operator to upload images: it did,
	// and the service now does it.
	if strings.Contains(joined, "upload images") {
		t.Errorf("closing step still tells the operator to upload images:\n%s", joined)
	}

	// The asset mutate must carry the BYTES, base64 encoded — the whole reason this
	// path exists is that Google will not fetch a URL.
	assetReq := readAsset()
	if strings.Contains(assetReq, srv.URL) {
		t.Error("the asset mutate sent the image URL; Google cannot fetch it")
	}
	if !strings.Contains(assetReq, `"imageAsset"`) || !strings.Contains(assetReq, `"data"`) {
		t.Errorf("asset mutate is not an image asset create: %s", assetReq)
	}
	wantPrefix := base64.StdEncoding.EncodeToString(pngOf(t, 1200, 628))[:32]
	if !strings.Contains(assetReq, wantPrefix) {
		t.Error("asset mutate does not carry the fetched image bytes")
	}

	// The ad must reference the resource names Google returned, each in the slot its
	// image came from — the marketing image as a marketing image, the logo as a logo.
	adReq := readAd()
	if !strings.Contains(adReq, `"marketingImages":[{"asset":"customers/1234567890/assets/900"}]`) {
		t.Errorf("ad does not reference the marketing asset in its own slot: %s", adReq)
	}
	if !strings.Contains(adReq, `"logoImages":[{"asset":"customers/1234567890/assets/901"}]`) {
		t.Errorf("ad does not reference the logo asset in its own slot: %s", adReq)
	}
	if !strings.Contains(adReq, `"businessName":"Linux Foundation"`) {
		t.Errorf("ad is missing the required business name: %s", adReq)
	}
	if !strings.Contains(adReq, `"callToActionText":"Register"`) {
		t.Errorf("ad is missing the call to action: %s", adReq)
	}
	// Created PAUSED like the Search ad, because the status cascade flips it with the
	// campaign when the operator launches.
	if !strings.Contains(adReq, `"status":"PAUSED"`) {
		t.Errorf("the ad must be created PAUSED: %s", adReq)
	}
}

// An image that fails its checks must fail BEFORE the budget mutate — the orphan
// guarantee this whole preflight exists for. A (nil, err) return is the proof:
// anything past the campaign create returns a non-nil partial result.
func TestCreateDemandGenCampaign_BadImageFailsBeforeAnyMutate(t *testing.T) {
	tooSmall := pngOf(t, 100, 100) // too small for every slot
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":mutate") {
			t.Errorf("a mutate was sent despite an unusable image: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(tooSmall)
	})
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err == nil {
		t.Fatal("an undersized image must fail the create")
	}
	if res != nil {
		t.Errorf("nothing was created, so the result must be nil, got %+v", res)
	}
}

// A creative that fails its PURE checks must be refused identically by the adoption
// path, which runs the same preflight and sends nothing at all.
func TestValidateCampaignInputKind_RefusesTheSameCreative(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	bad := fullCreative()
	bad.LogoImages = nil
	if err := c.ValidateCampaignInputKind(campaignKindDemandGen, creativeInput(bad)); err == nil {
		t.Error("adoption must refuse a creative the create path refuses")
	}
	if err := c.ValidateCampaignInputKind(campaignKindDemandGen, creativeInput(fullCreative())); err != nil {
		t.Errorf("adoption must accept a creative the create path accepts: %v", err)
	}
}

// Without a creative the cascade must behave exactly as it did before this feature:
// no asset mutate, no ad mutate, and a closing step that says the campaign cannot
// serve. Campaigns created before the creative existed are this shape.
func TestCreateDemandGenCampaign_NoCreativeKeepsTheOldShape(t *testing.T) {
	refuse := failHandler(t, "a creative mutate with no creative supplied")
	srv := demandGenTLSServer(t, demandGenCascade(t, nil, refuse, refuse))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), demandGenInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.AdID != "" || len(res.CreativeAssetIDs) != 0 {
		t.Errorf("no creative was asked for, yet the result carries ad/asset ids: %+v", res)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "NO AD") {
		t.Errorf("the closing step must say the campaign has no ad:\n%s", joined)
	}
}

// The assets are created but the ad fails: the asset ids must still come back, or
// the operator cannot find the account-level images nothing references.
func TestCreateDemandGenCampaign_AdFailureStillReportsTheAssets(t *testing.T) {
	assetH, _ := okCreativeAssets()
	adH := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH, adH))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err == nil {
		t.Fatal("the ad mutate failed; the create must report it")
	}
	// Past the campaign create, a failure must carry the partial result — never
	// (nil, err), which would release the claim on a campaign that exists.
	if res == nil {
		t.Fatal("a failure past the campaign create must return a non-nil partial result")
	}
	if len(res.CreativeAssetIDs) != 2 {
		t.Errorf("created-but-unreferenced assets must be reported, got %v", res.CreativeAssetIDs)
	}
	if res.AdID != "" {
		t.Errorf("the ad failed, so AdID must be empty, got %q", res.AdID)
	}
}

// Google answering with a resource name in a DIFFERENT account means the mutate is
// not accounted for: the ad must not be built from it, and the outcome is
// UNCONFIRMED rather than a plain failure, because something was created.
func TestCreateDemandGenCampaign_WrongAccountAssetIsUnconfirmed(t *testing.T) {
	assetH := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/9999999999/assets/900"},`+
			`{"resourceName":"customers/1234567890/assets/901"}]}`)
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH,
		failHandler(t, "the ad mutate after a wrong-account asset")))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("a wrong-account asset must be UNCONFIRMED, got: %v", err)
	}
	if res == nil {
		t.Fatal("a failure past the campaign create must return a non-nil partial result")
	}
}

// Google returning fewer asset results than operations sent means images are
// unaccounted for: the ad must not be built from a subset.
func TestCreateDemandGenCampaign_ShortAssetResponseIsUnconfirmed(t *testing.T) {
	assetH := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/assets/900"}]}`)
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH,
		failHandler(t, "the ad mutate after a short asset response")))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("a short asset response must be UNCONFIRMED, got: %v", err)
	}
	// The body PARSED — the one id in it is real, and is the only handle an operator
	// has on an account-level asset that may already exist. An UNCONFIRMED outcome
	// must not take it down with the error.
	if res == nil {
		t.Fatal("a failure past the campaign create must return a non-nil partial result")
	}
	if len(res.CreativeAssetIDs) != 1 || res.CreativeAssetIDs[0] != "900" {
		t.Errorf("the ids the short response DID carry must survive the error, got %v", res.CreativeAssetIDs)
	}
}

// A 5xx on the asset mutate is an UNKNOWN outcome, not a failure: the assets may
// have been created. Asserting "failed" over it invites the operator to retry into
// a second set of account-level image assets.
func TestCreateDemandGenCampaign_AssetMutate5xxIsUnconfirmed(t *testing.T) {
	assetH := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":500,"message":"backend error"}}`)
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH,
		failHandler(t, "the ad mutate after a 5xx asset mutate")))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("a 5xx on assets:mutate must be UNCONFIRMED, got: %v", err)
	}
	if res == nil {
		t.Fatal("a failure past the campaign create must return a non-nil partial result")
	}
}

// The other half of the same contract, stated so the ambiguity arm above cannot
// quietly widen into "every error is unconfirmed": a DEFINITE refusal stays a
// failure, because nothing was created and the claim can be released.
func TestCreateDemandGenCampaign_AssetMutate4xxIsStillAPlainFailure(t *testing.T) {
	assetH := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"invalid asset"}}`)
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH,
		failHandler(t, "the ad mutate after a 4xx asset mutate")))
	c := demandGenClient(t, srv)

	_, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err == nil {
		t.Fatal("a 400 on assets:mutate must fail the create")
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("a definite 400 must not be reported as UNCONFIRMED, got: %v", err)
	}
}

// Every error this path builds from a caller-supplied image URL must drop the query
// string. A signed CDN URL is the case that matters: the query IS the credential,
// and these errors are persisted unencrypted as Steps entries.
func TestValidateImageURLs_ErrorsNeverEchoTheQueryString(t *testing.T) {
	const secret = "SECRETSIGNATURE"
	signed := "https://cdn.example.org/a.png?sig=" + secret
	slot := demandGenImageSlots[0]

	cases := []struct {
		name string
		in   []string
	}{
		{"duplicate", []string{signed, signed}},
		{"no host", []string{"https:///a.png?sig=" + secret}},
		{"unparseable", []string{"https://cdn.example.org/\x7f?sig=" + secret}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateImageURLs(slot, tc.in)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error echoes the signed query string: %v", err)
			}
		})
	}
}

// The redirect TARGET is the same class of secret as the caller's own URL, reached by one
// hop. url.URL.Redacted() reads as though it handles that and does not — it masks only a
// password in userinfo and keeps the query, which for a signed CDN asset IS the credential.
func TestFetchOneImage_RedirectRefusalDoesNotEchoTheTargetsQuery(t *testing.T) {
	const secret = "SUPERSECRETSIGNATURE"
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cdn.example.org/img.png?X-Amz-Signature="+secret, http.StatusFound)
	})
	c := demandGenClient(t, srv)

	_, err := c.fetchOneImage(context.Background(), demandGenImageSlots[0], srv.URL+"/a.png?tok="+secret)
	if err == nil {
		t.Fatal("a redirect must be refused: a creative image URL has to point directly at the image")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal echoes a signed query string: %v", err)
	}
	if !strings.Contains(err.Error(), "cdn.example.org") {
		t.Errorf("the refusal must still name the host it refused to follow, for diagnosis: %v", err)
	}
}

// The aggregate cap is the only bound in the creative path that is STATEFUL across
// iterations: geometry, ratio, the per-image cap and the count bounds are each a pure
// function of one input and are covered by the table tests above. A running total is
// the kind of check that survives a refactor syntactically and dies semantically —
// reset per slot, scoped to the wrong block, or summed over the wrong value — while
// every other test still passes. Hence a direct test, and one that spans TWO slots, so
// the total is pinned as being across the whole creative rather than per slot.
func TestFetchSlotImages_TotalBytesCapIsAcrossEverySlot(t *testing.T) {
	// Padding after IEND: the decoder stops there, so these stay valid images while
	// carrying a known byte size. Each one is comfortably under the per-image cap, so
	// nothing but the SUM can refuse them.
	const each = 3 << 20
	if each > maxDemandGenImageBytes {
		t.Fatalf("fixture is wrong: a %d byte image is already over the per-image cap", each)
	}
	pad := bytes.Repeat([]byte{0}, each)
	wide := append(pngOf(t, 1200, 628), pad...)  // Demand Gen marketing image, 1.91:1
	square := append(pngOf(t, 600, 600), pad...) // Demand Gen square marketing image, 1:1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if strings.HasPrefix(r.URL.Path, "/square") {
			_, _ = w.Write(square)
			return
		}
		_, _ = w.Write(wide)
	}))
	t.Cleanup(srv.Close)

	c := imageFetchTestClient(t)
	slots := demandGenImageSlots[:2]

	// Split across the two slots so a per-slot accumulator would not trip while the
	// creative-wide one must.
	over := maxCreativeTotalImageBytes/each + 2
	urls := make([][]string, 2)
	for i := range over {
		if i%2 == 0 {
			urls[0] = append(urls[0], fmt.Sprintf("%s/wide%d.png", srv.URL, i))
		} else {
			urls[1] = append(urls[1], fmt.Sprintf("%s/square%d.png", srv.URL, i))
		}
	}
	if len(urls[0]) == 0 || len(urls[1]) == 0 {
		t.Fatalf("fixture is wrong: the %d images must span both slots", over)
	}

	if _, err := c.fetchSlotImages(context.Background(), slots, urls); err == nil {
		t.Errorf("%d images of %d bytes total more than the %d byte cap and must be refused", over, each, maxCreativeTotalImageBytes)
	} else if !strings.Contains(err.Error(), "across all slots") {
		t.Errorf("expected the creative-wide total refusal, got: %v", err)
	}

	// And the same rig one image short of the sum is accepted, so the refusal above is
	// the total firing rather than any per-image rule.
	under := [][]string{urls[0], urls[1]}
	if len(under[1]) > 0 {
		under[1] = under[1][:len(under[1])-1]
	}
	for len(under[0])+len(under[1]) > maxCreativeTotalImageBytes/each {
		under[0] = under[0][:len(under[0])-1]
	}
	got, err := c.fetchSlotImages(context.Background(), slots, under)
	if err != nil {
		t.Fatalf("%d images of %d bytes stay under the %d byte cap and must be accepted: %v", len(under[0])+len(under[1]), each, maxCreativeTotalImageBytes, err)
	}
	if len(got) != len(under[0])+len(under[1]) {
		t.Errorf("fetched %d images, want %d", len(got), len(under[0])+len(under[1]))
	}
}

// The fetch phase must not be able to spend the caller's whole budget. Per-image
// timeouts bound one image; only the phase cap bounds the walk, and the walk is what
// would otherwise leave the MUTATE cascade running against an exhausted deadline —
// the orphaned-campaign state the preflight design exists to avoid.
func TestFetchSlotImages_FetchPhaseCannotSpendTheWholeDeadline(t *testing.T) {
	body := pngOf(t, 1200, 628)
	const perImage = 600 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(perImage):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := imageFetchTestClient(t)
	slots := demandGenImageSlots[:1]
	urls := [][]string{{srv.URL + "/a.png", srv.URL + "/b.png", srv.URL + "/c.png"}}

	// Half of this is 1s, so the third image cannot land inside the phase share while
	// the caller's own deadline is still comfortably alive.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.fetchSlotImages(ctx, slots, urls)
	if err == nil {
		t.Fatal("the fetch phase must stop at its share of the deadline, not run on")
	}
	if !strings.Contains(err.Error(), "used its share of the deadline") {
		t.Errorf("expected the phase-budget refusal, got: %v", err)
	}
	if ctx.Err() != nil {
		t.Error("the caller's own deadline must survive the fetch phase — the mutate cascade still needs it")
	}
}

// And a caller with no deadline keeps the behaviour it had: the share is derived from
// what the caller actually has left, never invented, so nothing that used to fit is
// refused now.
func TestFetchSlotImages_NoCallerDeadlineIsLeftUnbounded(t *testing.T) {
	body := pngOf(t, 1200, 628)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := imageFetchTestClient(t)
	got, err := c.fetchSlotImages(context.Background(), demandGenImageSlots[:1], [][]string{{srv.URL + "/a.png"}})
	if err != nil {
		t.Fatalf("a caller with no deadline must fetch as before: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("fetched %d images, want 1", len(got))
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

// testPNG returns a w x h PNG.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func goodImage(t *testing.T) ImageInput {
	return ImageInput{Data: testPNG(t, 703, 368), MimeType: "image/png", AltText: "KubeCon NA 2026"}
}

// ---- validation -------------------------------------------------------------

func TestValidateImage(t *testing.T) {
	if err := ValidateImage(goodImage(t)); err != nil {
		t.Fatalf("a 703x368 PNG with alt text must pass: %v", err)
	}
	if err := ValidateImage(ImageInput{Data: testJPEG(t, 640, 360), MimeType: "image/jpeg", AltText: "x"}); err != nil {
		t.Fatalf("a JPEG must pass: %v", err)
	}
	if err := ValidateImage(ImageInput{Data: testPNG(t, 40, 20), MimeType: "image/png", AltText: "x"}); err != nil {
		t.Fatalf("the documented 40x20 minimum must pass: %v", err)
	}
	if err := ValidateImage(ImageInput{Data: testPNG(t, 2592, 2048), MimeType: "image/png", AltText: "x"}); err != nil {
		t.Fatalf("the documented 2592x2048 maximum must pass: %v", err)
	}
	big := make([]byte, MaxImageBytes+1)
	copy(big, testPNG(t, 100, 100))
	for name, in := range map[string]ImageInput{
		"empty":            {MimeType: "image/png", AltText: "x"},
		"over 76800 bytes": {Data: big, MimeType: "image/png", AltText: "x"},
		"gif mime":         {Data: testPNG(t, 100, 100), MimeType: "image/gif", AltText: "x"},
		"mime disagrees":   {Data: testPNG(t, 100, 100), MimeType: "image/jpeg", AltText: "x"},
		"undecodable":      {Data: []byte("not an image at all"), MimeType: "image/png", AltText: "x"},
		"too narrow":       {Data: testPNG(t, 39, 20), MimeType: "image/png", AltText: "x"},
		"too short":        {Data: testPNG(t, 40, 19), MimeType: "image/png", AltText: "x"},
		"too wide":         {Data: testPNG(t, 2593, 20), MimeType: "image/png", AltText: "x"},
		"too tall":         {Data: testPNG(t, 40, 2049), MimeType: "image/png", AltText: "x"},
		"blank alt":        {Data: testPNG(t, 100, 100), MimeType: "image/png", AltText: "   "},
		"alt over 35":      {Data: testPNG(t, 100, 100), MimeType: "image/png", AltText: strings.Repeat("a", 36)},
		"alt control char": {Data: testPNG(t, 100, 100), MimeType: "image/png", AltText: "a\x07b"},
		"alt format char":  {Data: testPNG(t, 100, 100), MimeType: "image/png", AltText: "a\u202eb"},
		"alt invalid utf8": {Data: testPNG(t, 100, 100), MimeType: "image/png", AltText: "a\xffb"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateImage(in)
			if !errors.Is(err, ErrImageInvalid) {
				t.Fatalf("want ErrImageInvalid, got %v", err)
			}
			if in.AltText != "" && len(in.AltText) > 3 && strings.Contains(err.Error(), in.AltText) {
				t.Errorf("the alt text must never be echoed: %v", err)
			}
		})
	}
	// 35 runes of a multi-byte character is within the limit (runes, not bytes).
	if err := ValidateImage(ImageInput{Data: testPNG(t, 100, 100), MimeType: "image/png", AltText: strings.Repeat("é", 35)}); err != nil {
		t.Errorf("35 characters must pass however many bytes they take: %v", err)
	}
}

func TestImageAltTextFromName(t *testing.T) {
	if got := ImageAltTextFromName("  " + strings.Repeat("é", 40)); got != strings.Repeat("é", 35) {
		t.Errorf("got %q, want 35 runes", got)
	}
	if got := ImageAltTextFromName("KubeCon"); got != "KubeCon" {
		t.Errorf("got %q", got)
	}
}

// ---- attach -----------------------------------------------------------------

const (
	noImageAssociations = `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[]}],"PartialErrors":[]}`
	nullSlotAssociation = `{"AdExtensionAssociationCollection":[null],"PartialErrors":null}`
)

// imageHappyPath answers every attach step successfully.
func imageHappyPath(_, path string) (int, string) {
	switch path {
	case "AdExtensionsAssociations/Query":
		return 200, noImageAssociations
	case "Media":
		return 200, `{"MediaIds":[5551]}`
	case "AdExtensions":
		return 200, `{"AdExtensionIdentities":[{"Id":7771,"Version":1}],"NestedPartialErrors":[]}`
	case "AdExtensionsAssociations/Set":
		return 200, `{"PartialErrors":[]}`
	}
	return 404, `{}`
}

func paths(calls []kwCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.path
	}
	return out
}

func TestAttachCampaignImage_Success(t *testing.T) {
	c, calls := kwRecorder(t, imageHappyPath)
	in := goodImage(t)
	r, err := c.AttachCampaignImage(context.Background(), "321", in)
	if err != nil {
		t.Fatalf("AttachCampaignImage: %v", err)
	}
	if r.MediaID != "5551" || r.AdExtensionID != "7771" || r.AlreadyAttached {
		t.Fatalf("result = %+v", r)
	}
	got := calls()
	want := []string{"AdExtensionsAssociations/Query", "Media", "AdExtensions", "AdExtensionsAssociations/Set"}
	if strings.Join(paths(got), ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", paths(got), want)
	}

	var media struct {
		AccountId json.Number
		Media     []map[string]string
	}
	if err := json.Unmarshal([]byte(got[1].body), &media); err != nil {
		t.Fatal(err)
	}
	if media.AccountId.String() != testAccount().AccountID || len(media.Media) != 1 {
		t.Fatalf("AddMedia body = %s", got[1].body)
	}
	m := media.Media[0]
	if m["Type"] != "Image" || m["MediaType"] != "GenericImage" {
		t.Errorf("media = Type %q MediaType %q, want Image/GenericImage", m["Type"], m["MediaType"])
	}
	if dec, err := base64.StdEncoding.DecodeString(m["Data"]); err != nil || !bytes.Equal(dec, in.Data) {
		t.Errorf("Data must be the standard base64 of the image bytes (err %v)", err)
	}

	var ext struct {
		AdExtensions []struct {
			Type            string
			AlternativeText string
			ImageMediaIds   []json.Number
			Layouts         []string
			DestinationUrl  *string
			FinalUrls       []string
		}
	}
	if err := json.Unmarshal([]byte(got[2].body), &ext); err != nil {
		t.Fatal(err)
	}
	e := ext.AdExtensions[0]
	if e.Type != "ImageAdExtension" || e.AlternativeText != "KubeCon NA 2026" ||
		len(e.ImageMediaIds) != 1 || e.ImageMediaIds[0].String() != "5551" ||
		len(e.Layouts) != 1 || e.Layouts[0] != "SearchSingle" {
		t.Errorf("extension body = %s", got[2].body)
	}
	if e.DestinationUrl != nil || e.FinalUrls != nil {
		t.Errorf("no URL may be sent — the extension inherits the ad's: %s", got[2].body)
	}

	var assoc struct {
		AdExtensionIdToEntityIdAssociations []struct{ AdExtensionId, EntityId json.Number }
		AssociationType                     string
	}
	if err := json.Unmarshal([]byte(got[3].body), &assoc); err != nil {
		t.Fatal(err)
	}
	if assoc.AssociationType != "Campaign" || len(assoc.AdExtensionIdToEntityIdAssociations) != 1 ||
		assoc.AdExtensionIdToEntityIdAssociations[0].AdExtensionId.String() != "7771" ||
		assoc.AdExtensionIdToEntityIdAssociations[0].EntityId.String() != "321" {
		t.Errorf("association body = %s", got[3].body)
	}
	if !strings.Contains(got[0].body, `"AdExtensionType":"ImageAdExtension"`) || !strings.Contains(got[0].body, `"EntityIds":[321]`) {
		t.Errorf("the lookup must be scoped to image extensions on this campaign: %s", got[0].body)
	}
}

// An image extension already associated with the campaign means a previous run attached one:
// nothing may be uploaded or created again.
func TestAttachCampaignImage_AlreadyAttachedUploadsNothing(t *testing.T) {
	c, calls := kwRecorder(t, func(m, p string) (int, string) {
		if p == "AdExtensionsAssociations/Query" {
			return 200, `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}]}]}`
		}
		return imageHappyPath(m, p)
	})
	r, err := c.AttachCampaignImage(context.Background(), "321", goodImage(t))
	if err != nil {
		t.Fatalf("AttachCampaignImage: %v", err)
	}
	if !r.AlreadyAttached || r.AdExtensionID != "4441" || r.MediaID != "" {
		t.Errorf("result = %+v", r)
	}
	if got := paths(calls()); len(got) != 1 {
		t.Errorf("only the lookup may run when an extension exists, got %v", got)
	}
}

// A null SLOT inside the collection array ([null]) is Microsoft's documented "none"; a literal
// null COLLECTION is unanswered and fails the lookup (TestAttachCampaignImage_LookupMustAnswer).
func TestAttachCampaignImage_NullSlotIsAbsent(t *testing.T) {
	c, calls := kwRecorder(t, func(m, p string) (int, string) {
		if p == "AdExtensionsAssociations/Query" {
			return 200, nullSlotAssociation
		}
		return imageHappyPath(m, p)
	})
	if _, err := c.AttachCampaignImage(context.Background(), "321", goodImage(t)); err != nil {
		t.Fatalf("AttachCampaignImage: %v", err)
	}
	if got := paths(calls()); len(got) != 4 {
		t.Errorf("a null collection is 'no associations', got %v", got)
	}
}

// A lookup that cannot rule out an existing extension must refuse to upload: AddMedia is not
// content-addressed, so uploading blind is how a retry double-uploads.
func TestAttachCampaignImage_LookupFailureUploadsNothing(t *testing.T) {
	for name, reply := range map[string]struct {
		status int
		body   string
	}{
		"5xx":                {502, `{}`},
		"undecodable 2xx":    {200, `not json`},
		"partial error":      {200, `{"AdExtensionAssociationCollection":[null],"PartialErrors":[{"Code":1001,"ErrorCode":"X"}]}`},
		"unusable id":        {200, `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":null,"Type":"ImageAdExtension"}}]}]}`},
		"definite 400 query": {400, `{"Code":100}`},
	} {
		t.Run(name, func(t *testing.T) {
			c, calls := kwRecorder(t, func(m, p string) (int, string) {
				if p == "AdExtensionsAssociations/Query" {
					return reply.status, reply.body
				}
				return imageHappyPath(m, p)
			})
			_, err := c.AttachCampaignImage(context.Background(), "321", goodImage(t))
			if err == nil {
				t.Fatal("a failed lookup must fail the attach")
			}
			if IsOutcomeUnconfirmed(err) {
				t.Errorf("nothing was mutated, so the failure is definite, not UNCONFIRMED: %v", err)
			}
			for _, cl := range calls() {
				if cl.path != "AdExtensionsAssociations/Query" {
					t.Errorf("no mutating call may follow a failed lookup, got %s", cl.path)
				}
			}
		})
	}
}

func TestAttachCampaignImage_StepFailuresAreClassified(t *testing.T) {
	const upstreamSecret = "UPSTREAM-MESSAGE-TEXT-must-not-leak"
	cases := []struct {
		name        string
		failPath    string
		status      int
		body        string
		unconfirmed bool
		wantMedia   string
		wantExt     string
		lastPath    string
	}{
		{"media 400 is definite", "Media", 400, `{"Code":100,"Message":"` + upstreamSecret + `"}`, false, "", "", "Media"},
		{"media 502 is unconfirmed", "Media", 502, `{"Message":"` + upstreamSecret + `"}`, true, "", "", "Media"},
		{"media 2xx without id is unconfirmed", "Media", 200, `{"MediaIds":[null]}`, true, "", "", "Media"},
		{"media 429 is unconfirmed and not retried", "Media", 429, `{}`, true, "", "", "Media"},
		{"extension partial error is definite", "AdExtensions", 200, `{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"Code":3000,"ErrorCode":"CampaignServiceInvalidImage","Message":"` + upstreamSecret + `"}]}`, false, "5551", "", "AdExtensions"},
		{"extension 500 is unconfirmed", "AdExtensions", 500, `{}`, true, "5551", "", "AdExtensions"},
		{"association partial error is definite", "AdExtensionsAssociations/Set", 200, `{"PartialErrors":[{"Code":3001,"ErrorCode":"X","Message":"` + upstreamSecret + `"}]}`, false, "5551", "7771", "AdExtensionsAssociations/Set"},
		{"association 503 is unconfirmed", "AdExtensionsAssociations/Set", 503, `{}`, true, "5551", "7771", "AdExtensionsAssociations/Set"},
		{"association unreadable 2xx is unconfirmed", "AdExtensionsAssociations/Set", 200, `nope`, true, "5551", "7771", "AdExtensionsAssociations/Set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := kwRecorder(t, func(m, p string) (int, string) {
				if p == tc.failPath {
					return tc.status, tc.body
				}
				return imageHappyPath(m, p)
			})
			r, err := c.AttachCampaignImage(context.Background(), "321", goodImage(t))
			if err == nil {
				t.Fatal("want an error")
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v (%v)", got, tc.unconfirmed, err)
			}
			if strings.Contains(err.Error(), upstreamSecret) {
				t.Errorf("upstream message text leaked into the error: %v", err)
			}
			if r == nil || r.MediaID != tc.wantMedia || r.AdExtensionID != tc.wantExt {
				t.Errorf("result = %+v, want media %q ext %q", r, tc.wantMedia, tc.wantExt)
			}
			got := calls()
			if n := len(got); got[n-1].path != tc.lastPath {
				t.Errorf("the attach must stop at the failed step, calls = %v", paths(got))
			}
			mediaCalls := 0
			for _, cl := range got {
				if cl.path == "Media" {
					mediaCalls++
				}
			}
			if mediaCalls != 1 {
				t.Errorf("AddMedia must be sent exactly once (a mutating 429 is not retried), got %d", mediaCalls)
			}
		})
	}
}

func TestAttachCampaignImage_RefusesBeforeAnyRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		campaign string
		in       ImageInput
	}{
		"bad campaign id": {"32a1", ImageInput{}},
		"invalid image":   {"321", ImageInput{Data: []byte("x"), MimeType: "image/png", AltText: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.in.Data == nil {
				tc.in = goodImage(t)
			}
			c, calls := kwRecorder(t, imageHappyPath)
			if _, err := c.AttachCampaignImage(context.Background(), tc.campaign, tc.in); err == nil {
				t.Fatal("want an error")
			}
			if n := len(calls()); n != 0 {
				t.Errorf("nothing may be sent, got %d requests", n)
			}
		})
	}
}

// The unconfirmed marker must survive wrapping, since the dispatcher wraps nothing but other
// callers may.
func TestImageUnconfirmedErrorUnwraps(t *testing.T) {
	err := classifyImageStep("x", &apiError{StatusCode: http.StatusBadGateway, Method: "POST", Path: "Media"})
	var ae *apiError
	if !errors.As(err, &ae) || !IsOutcomeUnconfirmed(err) {
		t.Errorf("classified error must unwrap to the apiError and report unconfirmed: %v", err)
	}
}

// ---- strict decoding (#307 review) ------------------------------------------

// runAttachWith overrides one path's reply and returns the attach outcome plus the calls.
func runAttachWith(t *testing.T, path string, status int, body string) (*ImageAttachResult, []kwCall, error) {
	t.Helper()
	c, calls := kwRecorder(t, func(m, p string) (int, string) {
		if p == path {
			return status, body
		}
		return imageHappyPath(m, p)
	})
	r, err := c.AttachCampaignImage(context.Background(), "321", goodImage(t))
	return r, calls(), err
}

// NestedPartialErrors is BatchErrorCollection[]: the failure may sit only in a collection's
// nested BatchErrors. A null identity explained there is a DEFINITE rejection; an identity
// returned alongside nested failures is not a clean success.
func TestAttachCampaignImage_NestedBatchErrors(t *testing.T) {
	t.Run("null identity + nested rejection is definite", func(t *testing.T) {
		r, _, err := runAttachWith(t, "AdExtensions", 200,
			`{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"Code":null,"BatchErrors":[{"Code":3001,"ErrorCode":"CampaignServiceInvalidImage","Index":0}]}]}`)
		if err == nil || IsOutcomeUnconfirmed(err) || !errors.Is(err, errPartialFailure) {
			t.Fatalf("want a definite rejection, got %v", err)
		}
		if !strings.Contains(err.Error(), "CampaignServiceInvalidImage") {
			t.Errorf("the nested code must be reported: %v", err)
		}
		if r.AdExtensionID != "" {
			t.Errorf("no extension id on a rejection, got %q", r.AdExtensionID)
		}
	})
	t.Run("identity + nested failure is not success", func(t *testing.T) {
		r, calls, err := runAttachWith(t, "AdExtensions", 200,
			`{"AdExtensionIdentities":[{"Id":7771}],"NestedPartialErrors":[{"BatchErrors":[{"Code":3002,"ErrorCode":"X","Index":0}]}]}`)
		if err == nil || !IsOutcomeUnconfirmed(err) {
			t.Fatalf("an identity with nested failures must be UNCONFIRMED, got %v", err)
		}
		if r.AdExtensionID != "7771" {
			t.Errorf("the returned id must still be recorded, got %q", r.AdExtensionID)
		}
		for _, cl := range calls {
			if cl.path == "AdExtensionsAssociations/Set" {
				t.Error("an extension Microsoft reported failures against must not be associated")
			}
		}
	})
	t.Run("collection-level code with null identity is definite", func(t *testing.T) {
		_, _, err := runAttachWith(t, "AdExtensions", 200,
			`{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"Code":3000,"ErrorCode":"Y","BatchErrors":null}]}`)
		if err == nil || IsOutcomeUnconfirmed(err) {
			t.Fatalf("want definite, got %v", err)
		}
	})
}

// An incomplete lookup body must never read as "no extension" — that is how a retry uploads
// a second image. The documented "none" shapes still mean none.
func TestAttachCampaignImage_LookupMustAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"collection omitted":    `{}`,
		"collection null":       `{"AdExtensionAssociationCollection":null}`,
		"collection not array":  `{"AdExtensionAssociationCollection":{}}`,
		"top-level null":        `null`,
		"other entity":          `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":999}]}]}`,
		"entity id missing":     `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"Campaign"}]}]}`,
		"ad group association":  `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"AdGroup","EntityId":321}]}]}`,
		"association type gone": `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"EntityId":321}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r, calls, err := runAttachWith(t, "AdExtensionsAssociations/Query", 200, body)
			if err == nil {
				t.Fatal("an unanswered or foreign lookup must fail the attach")
			}
			if IsOutcomeUnconfirmed(err) {
				t.Errorf("only a read was sent, so the failure is definite: %v", err)
			}
			if r.AlreadyAttached || r.AdExtensionID != "" {
				t.Errorf("a failed lookup is not AlreadyAttached: %+v", r)
			}
			if len(calls) != 1 {
				t.Errorf("nothing may be uploaded after a failed lookup, calls = %v", paths(calls))
			}
		})
	}
	for name, body := range map[string]string{
		"empty array":    `{"AdExtensionAssociationCollection":[]}`,
		"null slot":      `{"AdExtensionAssociationCollection":[null]}`,
		"empty slot":     `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":null}]}`,
		"no partial key": `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[]}]}`,
	} {
		t.Run("none: "+name, func(t *testing.T) {
			_, calls, err := runAttachWith(t, "AdExtensionsAssociations/Query", 200, body)
			if err != nil || len(calls) != 4 {
				t.Fatalf("a documented 'none' must proceed to attach, got %v, %v", err, paths(calls))
			}
		})
	}
}

func TestAttachCampaignImage_MediaPartialErrorIsDefinite(t *testing.T) {
	_, calls, err := runAttachWith(t, "Media", 200, `{"MediaIds":[null],"PartialErrors":[{"Code":1234,"ErrorCode":"InvalidMediaData","Index":0}]}`)
	if err == nil || IsOutcomeUnconfirmed(err) || !errors.Is(err, errPartialFailure) {
		t.Fatalf("an explained null MediaId is a definite rejection, got %v", err)
	}
	if paths(calls)[len(calls)-1] != "Media" {
		t.Errorf("must stop at AddMedia, calls = %v", paths(calls))
	}
}

func TestAttachCampaignImage_AssociationMustAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":   `{}`,
		"top-level null": `null`,
	} {
		t.Run(name, func(t *testing.T) {
			r, _, err := runAttachWith(t, "AdExtensionsAssociations/Set", 200, body)
			if err == nil || !IsOutcomeUnconfirmed(err) {
				t.Fatalf("a 2xx that never answered must be UNCONFIRMED, got %v", err)
			}
			if r.AdExtensionID != "7771" {
				t.Errorf("the extension id must be reported, got %q", r.AdExtensionID)
			}
		})
	}
	for name, body := range map[string]string{
		"partial null":  `{"PartialErrors":null}`,
		"partial empty": `{"PartialErrors":[]}`,
	} {
		t.Run("confirmed: "+name, func(t *testing.T) {
			if _, _, err := runAttachWith(t, "AdExtensionsAssociations/Set", 200, body); err != nil {
				t.Fatalf("an answered empty PartialErrors is success, got %v", err)
			}
		})
	}
}

// An UNCONFIRMED association must not carry a definite claim in its message.
func TestAttachCampaignImage_UnconfirmedAssociationMakesNoDefiniteClaim(t *testing.T) {
	_, _, err := runAttachWith(t, "AdExtensionsAssociations/Set", 503, `{}`)
	if err == nil || !IsOutcomeUnconfirmed(err) {
		t.Fatalf("want UNCONFIRMED, got %v", err)
	}
	if strings.Contains(err.Error(), "not attached") {
		t.Errorf("an unconfirmed outcome must not say 'not attached': %v", err)
	}
}

// ---- evidence vs truncation (#312 review) -----------------------------------

// nestedSlots renders n NestedPartialErrors entries, the entry at index realAt (if >= 0)
// carrying a real nested BatchError and the rest null placeholders.
func nestedSlots(n, realAt int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "null"
		if i == realAt {
			parts[i] = `{"BatchErrors":[{"Code":3001,"ErrorCode":"CampaignServiceInvalidImage","Index":0}]}`
		}
	}
	return `{"AdExtensionIdentities":[null],"NestedPartialErrors":[` + strings.Join(parts, ",") + `]}`
}

func TestAttachCampaignImage_TruncationAloneIsNotDefinite(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		definite bool
	}{
		{"17 null slots (over the cap, no error)", nestedSlots(maxDecodedErrorItems+1, -1), false},
		{"a few null slots", nestedSlots(3, -1), false},
		{"real error beyond the retained window", nestedSlots(maxDecodedErrorItems+4, maxDecodedErrorItems+2), true},
		{"real error retained", nestedSlots(3, 1), true},
		{"inner BatchErrors over the cap, real error beyond it", `{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"BatchErrors":[` +
			strings.Repeat("null,", maxDecodedErrorItems+2) + `{"Code":3001}]}]}`, true},
		{"inner BatchErrors over the cap, nulls only", `{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"BatchErrors":[` +
			strings.TrimSuffix(strings.Repeat("null,", maxDecodedErrorItems+3), ",") + `]}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runAttachWith(t, "AdExtensions", 200, tc.body)
			if err == nil {
				t.Fatal("a null identity is never success")
			}
			if got := !IsOutcomeUnconfirmed(err); got != tc.definite {
				t.Errorf("definite = %v, want %v (%v)", got, tc.definite, err)
			}
			if tc.definite != errors.Is(err, errPartialFailure) {
				t.Errorf("errPartialFailure must mark exactly the definite case: %v", err)
			}
		})
	}
}

func TestAttachCampaignImage_IdentityWithNullNestedEntryIsNotSuccess(t *testing.T) {
	r, _, err := runAttachWith(t, "AdExtensions", 200, `{"AdExtensionIdentities":[{"Id":7771}],"NestedPartialErrors":[null]}`)
	if err == nil || !IsOutcomeUnconfirmed(err) || r.AdExtensionID != "7771" {
		t.Fatalf("an identity beside a malformed nested entry must be UNCONFIRMED with its id, got %v, %+v", err, r)
	}
}

func TestAttachCampaignImage_AssociationNullEntriesAreUnconfirmed(t *testing.T) {
	for _, body := range []string{`{"PartialErrors":[null]}`, `{"PartialErrors":[{}]}`} {
		_, _, err := runAttachWith(t, "AdExtensionsAssociations/Set", 200, body)
		if err == nil || !IsOutcomeUnconfirmed(err) {
			t.Errorf("%s: a present error array with no codes must be UNCONFIRMED, got %v", body, err)
		}
	}
}

// ---- raw-body integrity (#312 review) ----------------------------------------

// A lookup body whose collection key is declared twice — exactly or under the decoder's case
// fold — could be marked present by one spelling and decoded empty from the other. It must fail
// the lookup, not read as "none".
func TestAttachCampaignImage_LookupRefusesDuplicateKeys(t *testing.T) {
	for name, body := range map[string]string{
		"exact duplicate":  `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}]}],"AdExtensionAssociationCollection":[]}`,
		"folded duplicate": `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}]}],"adextensionassociationcollection":[]}`,
		"malformed utf-8":  "{\"AdExtensionAssociationCollection\":[],\"x\":\"\xff\"}",
	} {
		t.Run(name, func(t *testing.T) {
			r, calls, err := runAttachWith(t, "AdExtensionsAssociations/Query", 200, body)
			if err == nil || IsOutcomeUnconfirmed(err) || r.AlreadyAttached {
				t.Fatalf("an untrustworthy lookup body must fail definitely, got %v, %+v", err, r)
			}
			if len(calls) != 1 {
				t.Errorf("nothing may be uploaded, calls = %v", paths(calls))
			}
		})
	}
}

func TestAttachCampaignImage_MutatingBodiesRefuseDuplicateKeys(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"Media", `{"MediaIds":[5551],"mediaids":[null]}`},
		{"AdExtensions", `{"AdExtensionIdentities":[{"Id":7771}],"NestedPartialErrors":[],"nestedpartialerrors":[{"Code":1}]}`},
		{"AdExtensionsAssociations/Set", `{"PartialErrors":[],"partialerrors":[{"Code":1}]}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, _, err := runAttachWith(t, tc.path, 200, tc.body)
			if err == nil || !IsOutcomeUnconfirmed(err) {
				t.Fatalf("an untrustworthy 2xx on a mutation must be UNCONFIRMED, got %v", err)
			}
		})
	}
}

// AddMedia classifies a null id on whole-wire evidence, like adExtensionIdentity.
func TestAttachCampaignImage_MediaEvidenceBeyondRetention(t *testing.T) {
	nulls := strings.Repeat("null,", maxDecodedErrorItems+1)
	cases := []struct {
		name     string
		body     string
		definite bool
	}{
		{"real error past the window", `{"MediaIds":[null],"PartialErrors":[` + nulls + `{"Code":3001,"ErrorCode":"InvalidMedia"}]}`, true},
		{"null placeholders only", `{"MediaIds":[null],"PartialErrors":[` + strings.TrimSuffix(nulls, ",") + `]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runAttachWith(t, "Media", 200, tc.body)
			if err == nil {
				t.Fatal("a null media id is never success")
			}
			if got := !IsOutcomeUnconfirmed(err); got != tc.definite {
				t.Errorf("definite = %v, want %v (%v)", got, tc.definite, err)
			}
		})
	}
}

// Every image association is validated, not just the first: a valid one followed by a foreign or
// unusable one must fail the lookup closed rather than read as AlreadyAttached.
func TestAttachCampaignImage_LookupValidatesEveryAssociation(t *testing.T) {
	const valid = `{"AdExtension":{"Id":4441,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}`
	for name, second := range map[string]string{
		"valid then foreign entity":             `{"AdExtension":{"Id":4442,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":999}`,
		"valid then ad group":                   `{"AdExtension":{"Id":4442,"Type":"ImageAdExtension"},"AssociationType":"AdGroup","EntityId":321}`,
		"valid then unusable id":                `{"AdExtension":{"Id":null,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}`,
		"valid then foreign in next collection": "",
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[` + valid + `,` + second + `]}]}`
			if second == "" {
				body = `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[` + valid + `]},{"AdExtensionAssociations":[{"AdExtension":{"Id":4442,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":999}]}]}`
			}
			r, calls, err := runAttachWith(t, "AdExtensionsAssociations/Query", 200, body)
			if err == nil || r.AlreadyAttached || r.AdExtensionID != "" {
				t.Fatalf("a mixed lookup must fail closed, got %v, %+v", err, r)
			}
			if IsOutcomeUnconfirmed(err) || len(calls) != 1 {
				t.Errorf("definite failure, nothing uploaded; got %v, calls %v", err, paths(calls))
			}
		})
	}
	t.Run("two valid associations keep the first", func(t *testing.T) {
		body := `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[` + valid + `,{"AdExtension":{"Id":4442,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}]}]}`
		r, calls, err := runAttachWith(t, "AdExtensionsAssociations/Query", 200, body)
		if err != nil || !r.AlreadyAttached || r.AdExtensionID != "4441" || len(calls) != 1 {
			t.Fatalf("got %v, %+v, %v", err, r, paths(calls))
		}
	})
}

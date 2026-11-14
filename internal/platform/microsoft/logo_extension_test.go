// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func goodLogo(t *testing.T) LogoInput {
	return LogoInput{Data: testPNG(t, 256, 256), MimeType: "image/png", BusinessName: " CNCF "}
}

func TestValidateLogo_AcceptsSquarePNGAndJPEGAtTheBounds(t *testing.T) {
	for name, in := range map[string]LogoInput{
		"png 256":  goodLogo(t),
		"png 128":  {Data: testPNG(t, MinLogoSide, MinLogoSide), MimeType: "image/png", BusinessName: "CNCF"},
		"jpeg 300": {Data: testJPEG(t, 300, 300), MimeType: "image/jpeg", BusinessName: strings.Repeat("a", MaxLogoBusinessNameRunes)},
		"png 2048": {Data: testPNG(t, MaxLogoSide, MaxLogoSide), MimeType: "image/png", BusinessName: "字字字"},
	} {
		if err := ValidateLogo(in); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateLogo_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		in   LogoInput
		want string
	}{
		"empty":         {LogoInput{MimeType: "image/png", BusinessName: "CNCF"}, "no bytes"},
		"too many byte": {LogoInput{Data: make([]byte, MaxImageBytes+1), MimeType: "image/png", BusinessName: "CNCF"}, "76801 bytes; Microsoft Advertising accepts at most 76800"},
		"video":         {LogoInput{Data: []byte("\x00\x00\x00\x18ftypmp42"), MimeType: "video/mp4", BusinessName: "CNCF"}, "only PNG and JPEG"},
		"gif mime":      {LogoInput{Data: testPNG(t, 200, 200), MimeType: "image/gif", BusinessName: "CNCF"}, "only PNG and JPEG"},
		"undecodable":   {LogoInput{Data: []byte("not an image"), MimeType: "image/png", BusinessName: "CNCF"}, "could not be decoded"},
		"mime mismatch": {LogoInput{Data: testJPEG(t, 200, 200), MimeType: "image/png", BusinessName: "CNCF"}, "not the recorded"},
		"not square":    {LogoInput{Data: testPNG(t, 300, 299), MimeType: "image/png", BusinessName: "CNCF"}, "300x299; a logo must be square"},
		"too small":     {LogoInput{Data: testPNG(t, 127, 127), MimeType: "image/png", BusinessName: "CNCF"}, "at least 128x128"},
		"too large":     {LogoInput{Data: testPNG(t, 2049, 2049), MimeType: "image/png", BusinessName: "CNCF"}, "at most 2048x2048"},
		"no name":       {LogoInput{Data: testPNG(t, 200, 200), MimeType: "image/png", BusinessName: "  "}, "business name is required"},
		"long name":     {LogoInput{Data: testPNG(t, 200, 200), MimeType: "image/png", BusinessName: strings.Repeat("a", 26)}, "26 characters; the limit is 25"},
		"control name":  {LogoInput{Data: testPNG(t, 200, 200), MimeType: "image/png", BusinessName: "CNCF\n"}, "control or format character"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateLogo(tc.in)
			if !errors.Is(err, ErrLogoInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrLogoInvalid containing %q", err, tc.want)
			}
		})
	}
}

// One association read, then AddMedia as an Image1x1, one LogoAdExtension naming the media id and
// the trimmed business name, and one Campaign association.
func TestAttachCampaignLogo_Success(t *testing.T) {
	c, calls := textExtRecorder(t, &textExtAPI{})
	in := goodLogo(t)
	r, err := c.AttachCampaignLogo(context.Background(), "321", in)
	if err != nil || r.AlreadyAttached || r.MediaID != "5551" || r.AdExtensionID != "9001" {
		t.Fatalf("got %+v, %v", r, err)
	}
	got := calls()
	if want := "AdExtensionsAssociations/Query,Media,AdExtensions,AdExtensionsAssociations/Set"; strings.Join(paths(got), ",") != want {
		t.Fatalf("calls = %v", paths(got))
	}
	if !strings.Contains(got[0].body, `"AdExtensionType":"LogoAdExtension"`) || !strings.Contains(got[0].body, `"AssociationType":"Campaign"`) {
		t.Errorf("lookup = %s", got[0].body)
	}
	var media struct {
		Media []struct{ Type, MediaType, Data string }
	}
	if err := json.Unmarshal([]byte(got[1].body), &media); err != nil || len(media.Media) != 1 {
		t.Fatalf("media body = %s (%v)", got[1].body, err)
	}
	if m := media.Media[0]; m.Type != "Image" || m.MediaType != "Image1x1" || m.Data != base64.StdEncoding.EncodeToString(in.Data) {
		t.Errorf("media = %+v", m)
	}
	const ext = `"AdExtensions":[{"Type":"LogoAdExtension","BusinessLogo":5551,"BusinessName":"CNCF"}]`
	if !strings.Contains(got[2].body, ext) {
		t.Errorf("extension body = %s\nwant %s", got[2].body, ext)
	}
	if !strings.Contains(got[3].body, `"AdExtensionId":9001,"EntityId":321`) || !strings.Contains(got[3].body, `"AssociationType":"Campaign"`) {
		t.Errorf("association body = %s", got[3].body)
	}
}

// A campaign that already has a logo gets nothing uploaded or added (a retry never doubles it).
func TestAttachCampaignLogo_SkipsWhenAlreadyAssociated(t *testing.T) {
	c, calls := textExtRecorder(t, &textExtAPI{existing: map[string]string{"LogoAdExtension": "4441"}})
	r, err := c.AttachCampaignLogo(context.Background(), "321", goodLogo(t))
	if err != nil || !r.AlreadyAttached || r.AdExtensionID != "4441" || r.MediaID != "" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if strings.Join(paths(calls()), ",") != "AdExtensionsAssociations/Query" {
		t.Errorf("calls = %v", paths(calls()))
	}
	if !strings.Contains(strings.Join(r.Steps, "\n"), "Logo extension already associated with the campaign: 4441 (nothing uploaded)") {
		t.Errorf("steps = %v", r.Steps)
	}
}

// A lookup that cannot answer uploads nothing, and the failure is definite (only a read was sent).
func TestAttachCampaignLogo_LookupFailureUploadsNothing(t *testing.T) {
	c, calls := textExtRecorder(t, &textExtAPI{override: func(p, _ string) (int, string, bool) {
		return 502, `{}`, p == "AdExtensionsAssociations/Query"
	}})
	_, err := c.AttachCampaignLogo(context.Background(), "321", goodLogo(t))
	if err == nil || IsOutcomeUnconfirmed(err) {
		t.Fatalf("err = %v, want a definite failure", err)
	}
	if n := len(calls()); n != 1 {
		t.Errorf("%d calls, want the one lookup", n)
	}
}

// A 5xx on AddMedia is UNCONFIRMED; a 400 on the extension create is definite and keeps the media id.
func TestAttachCampaignLogo_ClassifiesFailures(t *testing.T) {
	c, _ := textExtRecorder(t, &textExtAPI{override: func(p, _ string) (int, string, bool) {
		return 503, `{}`, p == "Media"
	}})
	r, err := c.AttachCampaignLogo(context.Background(), "321", goodLogo(t))
	if !IsOutcomeUnconfirmed(err) || r.AdExtensionID != "" {
		t.Fatalf("media 5xx: %+v, %v", r, err)
	}
	c, calls := textExtRecorder(t, &textExtAPI{override: func(p, _ string) (int, string, bool) {
		return 400, `{"Code":1001,"ErrorCode":"InvalidRequest"}`, p == "AdExtensions"
	}})
	r, err = c.AttachCampaignLogo(context.Background(), "321", goodLogo(t))
	if err == nil || IsOutcomeUnconfirmed(err) || r.MediaID != "5551" {
		t.Fatalf("extension 400: %+v, %v", r, err)
	}
	for _, call := range calls() {
		if call.path == "AdExtensionsAssociations/Set" {
			t.Error("a refused extension must not be associated")
		}
	}
}

func TestAttachCampaignLogo_RefusesBeforeAnyRequest(t *testing.T) {
	c, calls := textExtRecorder(t, &textExtAPI{})
	if _, err := c.AttachCampaignLogo(context.Background(), "321", LogoInput{Data: testPNG(t, 300, 200), MimeType: "image/png", BusinessName: "CNCF"}); !errors.Is(err, ErrLogoInvalid) {
		t.Fatalf("a non-square logo must be refused, got %v", err)
	}
	if _, err := c.AttachCampaignLogo(context.Background(), "x", goodLogo(t)); err == nil {
		t.Fatal("a bad campaign id must be refused")
	}
	if len(calls()) != 0 {
		t.Errorf("calls = %v, want none", paths(calls()))
	}
}

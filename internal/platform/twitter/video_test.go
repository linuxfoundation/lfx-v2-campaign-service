// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// testMP4 is n bytes opening with an ftyp box (enough for the client's re-check; the store checks
// the full structure at upload).
func testMP4(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x00\x00\x00\x18ftypmp42")
	for i := 12; i < n; i++ {
		b[i] = byte(i % 241)
	}
	return b
}

func videoInput(v []byte) CampaignInput {
	in := baseAuthorInput("Watch the KubeCon keynote")
	in.Video = &VideoInput{Bytes: v, MIME: "video/mp4"}
	return in
}

// The happy path: the MP4 is uploaded in segments as amplify_video (video/mp4), STATUS is polled
// while X processes it, it is added to the Media Library under its SHA-256 name as a VIDEO, all
// before the campaign, and its key rides on the authored tweet as media_keys.
func TestCreateCampaign_VideoChunkedUploadAttachesMediaKey(t *testing.T) {
	f := newMediaFakeX()
	f.wantCategory = "amplify_video"
	f.finalizeBody = `{"data":{"id":"777","media_key":"7_777","processing_info":{"state":"pending","check_after_secs":1}}}`
	f.statusBody = `{"data":{"id":"777","media_key":"7_777","processing_info":{"state":"succeeded"}}}`
	c := newMediaTestClient(t, f)
	c.mediaChunkSize = 1000
	v := testMP4(2600)
	res, err := c.CreateCampaign(context.Background(), videoInput(v))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !bytes.Equal(f.uploaded(), v) || f.count("append") != 3 || f.count("status") != 1 {
		t.Fatalf("appends=%d status=%d, want 3 segments reassembling the video and one STATUS check", f.count("append"), f.count("status"))
	}
	f.mu.Lock()
	order, tweetQuery, initType, lookupQuery, libraryQuery := append([]string(nil), f.order...), f.tweetQuery, f.initMediaType, f.lookupQuery, f.libraryQuery
	f.mu.Unlock()
	if initType != "video/mp4" || !strings.Contains(lookupQuery, "media_type=VIDEO") || !strings.Contains(libraryQuery, "name="+mediaLibraryName(v)) {
		t.Errorf("init type %q, lookup %q, library %q", initType, lookupQuery, libraryQuery)
	}
	if !strings.Contains(tweetQuery, "media_keys=7_777") || res.MediaKey != "7_777" {
		t.Errorf("tweet query %q, MediaKey %q, want the video key", tweetQuery, res.MediaKey)
	}
	joined := strings.Join(order, ",")
	if strings.Index(joined, "library") > strings.Index(joined, "campaign") {
		t.Fatalf("the video must be in the library before the campaign POST: %v", order)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Creative video uploaded (media 777)") {
		t.Errorf("steps lack the video upload: %v", res.Steps)
	}
}

// Retry safety: a video already in the library under its name is reused — nothing is uploaded.
func TestCreateCampaign_VideoInTheLibraryIsNotReuploaded(t *testing.T) {
	f := newMediaFakeX()
	v := testMP4(900)
	f.library[mediaLibraryName(v)] = "7_555"
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), videoInput(v))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if f.count("init")+f.count("append")+f.count("library") != 0 || res.MediaKey != "7_555" {
		t.Fatalf("uploaded again (init=%d library=%d) or wrong key %q", f.count("init"), f.count("library"), res.MediaKey)
	}
}

// Processing that does not finish within the bound fails the create with NOTHING created: no
// library entry, no campaign, no tweet. The STATUS checks stop at maxVideoStatusPolls.
func TestCreateCampaign_VideoProcessingIsBoundedAndCreatesNothing(t *testing.T) {
	for name, status := range map[string]string{
		"still processing": `{"data":{"id":"777","processing_info":{"state":"in_progress","check_after_secs":1}}}`,
		"failed":           `{"data":{"id":"777","processing_info":{"state":"failed"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newMediaFakeX()
			f.wantCategory = "amplify_video"
			f.finalizeBody = `{"data":{"id":"777","media_key":"7_777","processing_info":{"state":"pending","check_after_secs":1}}}`
			f.statusBody = status
			c := newMediaTestClient(t, f)
			res, err := c.CreateCampaign(context.Background(), videoInput(testMP4(500)))
			if res != nil || err == nil || !strings.Contains(err.Error(), "creative video") {
				t.Fatalf("res=%v err=%v, want (nil, a video error)", res, err)
			}
			if f.count("library")+f.count("campaign")+f.count("tweet") != 0 {
				t.Fatalf("library=%d campaign=%d tweet=%d after a failed video, want none", f.count("library"), f.count("campaign"), f.count("tweet"))
			}
			if n := f.count("status"); n > maxVideoStatusPolls {
				t.Fatalf("%d STATUS checks, more than the %d bound", n, maxVideoStatusPolls)
			}
		})
	}
}

// The video is the website card's media when a card is requested.
func TestCreateCampaign_VideoIsTheCardMedia(t *testing.T) {
	f := newMediaFakeX()
	f.wantCategory = "amplify_video"
	f.finalizeBody = `{"data":{"id":"777","media_key":"7_777"}}`
	c := newMediaTestClient(t, f)
	in := videoInput(testMP4(500))
	in.WebsiteCard = &WebsiteCardInput{Headline: "Keynote"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	f.mu.Lock()
	body := f.cardBody
	f.mu.Unlock()
	if !strings.Contains(body, `"media_key":"7_777"`) {
		t.Fatalf("card body %s, want the video's media_key", body)
	}
}

// Refused before any request: a video with an image, without an authored tweet, of the wrong type
// or not an MP4, and a create naming more criteria than a video leaves room for.
func TestCreateCampaign_VideoRefusedBeforeAnyRequest(t *testing.T) {
	over := make([]string, MaxCreateTargetingCriteriaWithVideo+1)
	for i := range over {
		over[i] = fmt.Sprint(i + 1)
	}
	for name, mut := range map[string]func(*CampaignInput){
		"with an image":  func(in *CampaignInput) { in.Image = &ImageInput{Bytes: testPNG(100), MIME: "image/png"} },
		"explicit tweet": func(in *CampaignInput) { in.TweetID = "123" },
		"wrong type":     func(in *CampaignInput) { in.Video.MIME = "video/quicktime" },
		"not an mp4":     func(in *CampaignInput) { in.Video.Bytes = testPNG(100) },
		"too large":      func(in *CampaignInput) { in.Video.Bytes = testMP4(MaxVideoBytes + 1) },
		"over criteria":  func(in *CampaignInput) { in.FollowerLookalikeUserIDs = over },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMediaFakeX()
			c := newMediaTestClient(t, f)
			in := videoInput(testMP4(500))
			mut(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || !isRefusal(err) {
				t.Fatalf("res=%v err=%v, want a pre-create refusal", res, err)
			}
			f.mu.Lock()
			n := len(f.order)
			f.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d requests before the refusal; want 0", n)
			}
		})
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadImage_HappyPathReturnsHostedURL(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake-png-bytes"))
	}))
	t.Cleanup(imgSrv.Close)

	var gotFilename, gotFolderPath, gotOptions string
	var gotAuth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != filesPath {
			t.Errorf("unexpected upload path %s", r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("expected multipart content-type, got %q (%v)", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				t.Fatalf("read multipart part: %v", perr)
			}
			switch part.FormName() {
			case "file":
				gotFilename = part.FileName()
				data, _ := io.ReadAll(part)
				if string(data) != "fake-png-bytes" {
					t.Errorf("uploaded bytes = %q, want fake-png-bytes", data)
				}
			case "folderPath":
				b, _ := io.ReadAll(part)
				gotFolderPath = string(b)
			case "options":
				b, _ := io.ReadAll(part)
				gotOptions = string(b)
			}
		}
		_, _ = io.WriteString(w, `{"url":"https://hubspot.example/hubfs/hero.png"}`)
	})

	got, err := c.UploadImage(context.Background(), imgSrv.URL+"/hero-source.png")
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if got != "https://hubspot.example/hubfs/hero.png" {
		t.Errorf("UploadImage returned %q", got)
	}
	if gotAuth != "Bearer pat-test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotFilename != "hero-source.png" {
		t.Errorf("filename = %q, want hero-source.png", gotFilename)
	}
	if gotFolderPath != "/email-staging" {
		t.Errorf("folderPath = %q, want /email-staging", gotFolderPath)
	}
	if !strings.Contains(gotOptions, `"access":"PUBLIC_INDEXABLE"`) {
		t.Errorf("options = %q, missing PUBLIC_INDEXABLE access", gotOptions)
	}
}

func TestUploadImage_RejectsNonImageContentType(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	t.Cleanup(imgSrv.Close)

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upload call should be made when the source URL isn't an image")
	})

	_, err := c.UploadImage(context.Background(), imgSrv.URL+"/not-an-image")
	if err == nil {
		t.Fatal("expected an error for a non-image content-type")
	}
}

func TestUploadImage_RejectsEmptyURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call should be made for an empty image URL")
	})
	if _, err := c.UploadImage(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for an empty image URL")
	}
}

func TestUploadImage_NonSuccessUploadStatusIsAnAPIError(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("bytes"))
	}))
	t.Cleanup(imgSrv.Close)

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"forbidden"}`)
	})

	_, err := c.UploadImage(context.Background(), imgSrv.URL+"/hero.jpg")
	if err == nil {
		t.Fatal("expected an error on a non-2xx upload response")
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *apiError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
}

func TestDeriveImageFilename(t *testing.T) {
	cases := []struct {
		url, contentType, want string
	}{
		{"https://cdn.example.com/path/hero-banner.jpg", "image/jpeg", "hero-banner.jpg"},
		{"https://cdn.example.com/path/", "image/png", "email_img.png"},
		{"https://cdn.example.com/no-extension", "image/jpeg", "email_img.jpg"},
	}
	for _, tc := range cases {
		if got := deriveImageFilename(tc.url, tc.contentType); got != tc.want {
			t.Errorf("deriveImageFilename(%q, %q) = %q, want %q", tc.url, tc.contentType, got, tc.want)
		}
	}
}

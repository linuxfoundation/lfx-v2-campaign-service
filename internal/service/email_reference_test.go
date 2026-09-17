// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// ---- fakes ------------------------------------------------------------------

// fakeConnReader answers Get with a preset connection per project id, or ErrNotFound
// when the project isn't in the map — mirroring internal/dispatch's fakeConnReader
// (reddit_test.go) closely enough to test the same fallback shape without importing
// internal/dispatch (see hubspotCreds' doc comment for why that import is a cycle).
type fakeConnReader struct {
	byProject map[string]*model.Connection
}

func (f fakeConnReader) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	conn, ok := f.byProject[projectID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return conn, nil
}

func (f fakeConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// identityEncryptor treats ciphertext as plaintext, so tests can put readable JSON in
// EncryptedCredentials.
type identityEncryptor struct{}

func (identityEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (identityEncryptor) Decrypt(c []byte) ([]byte, error) { return c, nil }

// errDecryptEncryptor always fails Decrypt.
type errDecryptEncryptor struct{}

func (errDecryptEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (errDecryptEncryptor) Decrypt([]byte) ([]byte, error)   { return nil, errors.New("bad key") }

func credsBlob(t *testing.T, token string) []byte {
	t.Helper()
	b, err := json.Marshal(hubspotCreds{PrivateAppToken: token})
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}
	return b
}

func activeConn(t *testing.T, token string) *model.Connection {
	t.Helper()
	return &model.Connection{
		Provider:             model.ProviderHubSpot,
		Status:               model.StatusActive,
		EncryptedCredentials: credsBlob(t, token),
		ProviderConfig:       map[string]string{"portal_id": "12345"},
	}
}

// widgetJSON builds one rich-text widget's JSON body (see hubspot's widgetBody.html).
func widgetJSON(html string) string {
	b, _ := json.Marshal(map[string]any{"body": map[string]any{"html": html}})
	return string(b)
}

// emailDraftServer builds an httptest.Server that answers HubSpot's SearchEmails
// (GET /marketing/v3/emails) with searchResults, and GetEmailHTMLWidgets
// (GET /marketing/v3/emails/{id}/draft) with the placed rich-text blocks named in
// draftsByID (id -> html strings, in reading order).
func emailDraftServer(t *testing.T, searchResults string, draftsByID map[string][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/draft") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/marketing/v3/emails/"), "/draft")
			blocks, ok := draftsByID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
				return
			}
			widgets := map[string]any{}
			var order []string
			for i, html := range blocks {
				key := fmt.Sprintf("w%d", i)
				widgets[key] = json.RawMessage(widgetJSON(html))
				order = append(order, key)
			}
			body := fmt.Sprintf(
				`{"content":{"widgets":%s,"flexAreas":{"main":{"sections":[{"columns":[{"widgets":%s}]}]}}}}`,
				mustMarshalJSON(t, widgets), mustMarshalJSON(t, order),
			)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(searchResults))
	}))
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ---- tests --------------------------------------------------------------------

func TestBuildReferenceBlock_HappyPath(t *testing.T) {
	srv := emailDraftServer(t,
		`{"results":[
			{"id":"1","subject":"Rich Template","state":"PUBLISHED","updatedAt":"2026-01-03T00:00:00Z"},
			{"id":"2","subject":"Thin Template","state":"PUBLISHED","updatedAt":"2026-01-02T00:00:00Z"},
			{"id":"3","subject":"A Draft","state":"DRAFT","updatedAt":"2026-01-01T00:00:00Z"}
		]}`,
		map[string][]string{
			"1": {"Join us for KubeCon", "See you there, register today!"},
			"2": {"Short note"},
		},
	)
	defer srv.Close()

	conn := activeConn(t, "tok-123")
	src := NewEmailReferenceSource(
		fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}},
		identityEncryptor{},
		hubspot.WithBaseURL(srv.URL),
	)

	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got == "" {
		t.Fatal("expected a non-empty reference block")
	}
	if !strings.Contains(got, `subject: "Rich Template"`) {
		t.Errorf("richest candidate (email 1) must be the primary template, got: %s", got)
	}
	if !strings.Contains(got, "Join us for KubeCon") {
		t.Errorf("primary excerpt missing its text, got: %s", got)
	}
	if !strings.Contains(got, "Additional style/tone samples") || !strings.Contains(got, "Short note") {
		t.Errorf("the thinner candidate must appear in the style corpus, got: %s", got)
	}
	if strings.Contains(got, "A Draft") {
		t.Errorf("a DRAFT email must never contribute to the block, got: %s", got)
	}
}

func TestBuildReferenceBlock_FallsBackToSystemConnection(t *testing.T) {
	srv := emailDraftServer(t,
		`{"results":[{"id":"1","subject":"System Template","state":"PUBLISHED","updatedAt":"2026-01-01T00:00:00Z"}]}`,
		map[string][]string{"1": {"Hello from the shared portal"}},
	)
	defer srv.Close()

	systemConn := activeConn(t, "system-tok")
	src := NewEmailReferenceSource(
		fakeConnReader{byProject: map[string]*model.Connection{model.SystemProjectID: systemConn}},
		identityEncryptor{},
		hubspot.WithBaseURL(srv.URL),
	)

	got := src.BuildReferenceBlock(context.Background(), "proj-no-own-connection")
	if !strings.Contains(got, "System Template") {
		t.Errorf("expected the system connection's email to be used, got: %q", got)
	}
}

func TestBuildReferenceBlock_NoConnectionAnywhereReturnsEmpty(t *testing.T) {
	src := NewEmailReferenceSource(fakeConnReader{byProject: map[string]*model.Connection{}}, identityEncryptor{})
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("expected empty block with no connection at all, got: %q", got)
	}
}

func TestBuildReferenceBlock_InactiveConnectionReturnsEmpty(t *testing.T) {
	conn := activeConn(t, "tok")
	conn.Status = "revoked"
	src := NewEmailReferenceSource(fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}}, identityEncryptor{})
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("expected empty block for an inactive connection, got: %q", got)
	}
}

func TestBuildReferenceBlock_NoCredentialsReturnsEmpty(t *testing.T) {
	conn := &model.Connection{Provider: model.ProviderHubSpot, Status: model.StatusActive}
	src := NewEmailReferenceSource(fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}}, identityEncryptor{})
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("expected empty block for a connection with no stored credentials, got: %q", got)
	}
}

func TestBuildReferenceBlock_DecryptFailureReturnsEmpty(t *testing.T) {
	conn := activeConn(t, "tok")
	src := NewEmailReferenceSource(fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}}, errDecryptEncryptor{})
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("expected empty block when decryption fails, got: %q", got)
	}
}

func TestBuildReferenceBlock_EmptyTokenReturnsEmpty(t *testing.T) {
	conn := activeConn(t, "   ")
	src := NewEmailReferenceSource(fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}}, identityEncryptor{})
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("expected empty block for a blank private-app token, got: %q", got)
	}
}

func TestBuildReferenceBlock_SearchFailureReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	conn := activeConn(t, "tok")
	src := NewEmailReferenceSource(
		fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}},
		identityEncryptor{},
		hubspot.WithBaseURL(srv.URL),
	)
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("a HubSpot search failure must be swallowed (best-effort), got: %q", got)
	}
}

func TestBuildReferenceBlock_NoPublishedEmailsReturnsEmpty(t *testing.T) {
	srv := emailDraftServer(t,
		`{"results":[{"id":"1","subject":"Still a draft","state":"DRAFT","updatedAt":"2026-01-01T00:00:00Z"}]}`,
		map[string][]string{"1": {"Draft body"}},
	)
	defer srv.Close()

	conn := activeConn(t, "tok")
	src := NewEmailReferenceSource(
		fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}},
		identityEncryptor{},
		hubspot.WithBaseURL(srv.URL),
	)
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("a portal with only drafts must yield an empty block, got: %q", got)
	}
}

func TestBuildReferenceBlock_UnplacedOnlyEmailIsSkipped(t *testing.T) {
	// A classic (non-drag-and-drop) template places nothing, so plainTextFromBlocks
	// (which only counts Placed blocks) returns "" for it — the candidate must then be
	// dropped rather than contributing an empty excerpt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/draft") {
			_, _ = w.Write([]byte(`{"content":{"widgets":{"w0":` + widgetJSON("Unplaced body") + `},"flexAreas":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"1","subject":"Classic","state":"PUBLISHED","updatedAt":"2026-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	conn := activeConn(t, "tok")
	src := NewEmailReferenceSource(
		fakeConnReader{byProject: map[string]*model.Connection{"proj-1": conn}},
		identityEncryptor{},
		hubspot.WithBaseURL(srv.URL),
	)
	got := src.BuildReferenceBlock(context.Background(), "proj-1")
	if got != "" {
		t.Errorf("a classic template with no placed blocks must be skipped, got: %q", got)
	}
}

func TestPublishedEmails_FiltersDraftsCaseInsensitively(t *testing.T) {
	in := []hubspot.Email{
		{ID: "1", State: "PUBLISHED"},
		{ID: "2", State: "published"},
		{ID: "3", State: "DRAFT"},
		{ID: "4", State: "DRAFT_AB_VARIANT"},
		{ID: "5", State: ""},
	}
	got := publishedEmails(in)
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("expected only the two published emails in original order, got %+v", got)
	}
}

func TestPlainTextFromBlocks_OnlyPlacedNonEmptyBlocksInOrder(t *testing.T) {
	blocks := []hubspot.EmailHTMLBlock{
		{Key: "footer", HTML: "<p>Unsubscribe</p>", Placed: false},
		{Key: "hero", HTML: "<h1>Hello&nbsp;World</h1>", Placed: true},
		{Key: "empty", HTML: "   ", Placed: true},
		{Key: "body", HTML: "<p>Join &amp; register</p>", Placed: true},
	}
	got := plainTextFromBlocks(blocks)
	want := "Hello World\nJoin & register"
	if got != want {
		t.Errorf("plainTextFromBlocks() = %q, want %q", got, want)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Errorf("short string must pass through unchanged, got %q", got)
	}
	if got := truncateRunes("hello world", 5); got != "hello" {
		t.Errorf("truncateRunes(11-char, 5) = %q, want %q", got, "hello")
	}
	// Multi-byte runes: counted as runes, not bytes, so a cut lands on a rune boundary.
	jp := strings.Repeat("こ", 5)
	if got := truncateRunes(jp, 3); got != strings.Repeat("こ", 3) {
		t.Errorf("truncateRunes must cut on rune boundaries, got %q", got)
	}
}

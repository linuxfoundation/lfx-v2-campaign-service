// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
)

// requestCapture hands what a handler saw to the test goroutine. The handler runs on the
// httptest server's goroutine, so a plain captured variable is a data race, and a t.Fatalf from
// there is FailNow off the test goroutine. This records under a lock, keeps a decode failure as a
// value, and leaves every assertion to the test.
type requestCapture struct {
	mu     sync.Mutex
	path   string
	method string
	body   map[string]any
	err    error
}

// record notes the request line and, when withBody is set, its JSON body.
func (c *requestCapture) record(r *http.Request, withBody bool) {
	var body map[string]any
	var err error
	if withBody {
		err = json.NewDecoder(r.Body).Decode(&body)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.method, c.body, c.err = r.URL.Path, r.Method, body, err
}

// seen returns the last recorded request; it fails the test, on the test goroutine, when that
// request's body could not be decoded.
func (c *requestCapture) seen(t *testing.T) (path, method string, body map[string]any) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		t.Fatalf("decode request body: %v", c.err)
	}
	return c.path, c.method, c.body
}

// The two recipient settings travel in the SAME write as the lists, because the `to` object is
// written whole: a PATCH naming only a setting would be a complete `to` with no recipients in it.
func TestSetSendListWithSettings_StatesTheSettingsBesideTheLists(t *testing.T) {
	var captured requestCapture
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured.record(r, true)
		_, _ = io.WriteString(w, `{"id":"999","to":{"suppressGraymail":false,"limitSendFrequency":true}}`)
	})
	off, on := false, true
	email, reported, err := c.SetSendListWithSettings(context.Background(), "999", []string{"3797"}, []string{"3732"},
		SendSettings{SuppressGraymail: &off, LimitSendFrequency: &on})
	if err != nil {
		t.Fatalf("SetSendListWithSettings: %v", err)
	}
	gotPath, _, body := captured.seen(t)
	if gotPath != "/marketing/v3/emails/999/draft" || email == nil || email.ID != "999" {
		t.Errorf("the write must PATCH the draft route and return the email, got path %q email %+v", gotPath, email)
	}
	to, _ := body["to"].(map[string]any)
	if got, stated := to["suppressGraymail"].(bool); !stated || got {
		t.Errorf("to.suppressGraymail = %v, want false stated", to["suppressGraymail"])
	}
	if got, stated := to["limitSendFrequency"].(bool); !stated || !got {
		t.Errorf("to.limitSendFrequency = %v, want true stated", to["limitSendFrequency"])
	}
	ils, _ := to["contactIlsLists"].(map[string]any)
	if include, _ := ils["include"].([]any); len(include) != 1 || include[0] != "3797" {
		t.Errorf("the lists must travel in the same write, got %v", to["contactIlsLists"])
	}
	if reported.SuppressGraymail == nil || *reported.SuppressGraymail || reported.LimitSendFrequency == nil || !*reported.LimitSendFrequency {
		t.Errorf("reported settings = %+v, want what the portal answered with", reported)
	}
}

// A setting that is not given is not written: HubSpot decides it, as for every email that is not
// a newsletter issue. Stating one here would change the recipients of sends this client has
// always left alone.
func TestSetSendList_LeavesTheSettingsToHubSpot(t *testing.T) {
	var captured requestCapture
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured.record(r, true)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	if _, err := c.SetSendList(context.Background(), "999", []string{"3797"}, nil); err != nil {
		t.Fatalf("SetSendList: %v", err)
	}
	_, _, body := captured.seen(t)
	to, _ := body["to"].(map[string]any)
	for _, key := range []string{"suppressGraymail", "limitSendFrequency"} {
		if _, stated := to[key]; stated {
			t.Errorf("SetSendList must not state %s, got %v", key, to[key])
		}
	}

	// One setting given, the other not: only the given one is written.
	on := true
	_, reported, err := c.SetSendListWithSettings(context.Background(), "999", []string{"3797"}, nil, SendSettings{SuppressGraymail: &on})
	if err != nil {
		t.Fatalf("SetSendListWithSettings: %v", err)
	}
	_, _, body = captured.seen(t)
	to, _ = body["to"].(map[string]any)
	if _, stated := to["limitSendFrequency"]; stated {
		t.Errorf("a nil setting must be left out, got %v", to["limitSendFrequency"])
	}
	if got, stated := to["suppressGraymail"].(bool); !stated || !got {
		t.Errorf("to.suppressGraymail = %v, want true stated", to["suppressGraymail"])
	}
	// The answer carried no `to`, so nothing is reported rather than a false "off".
	if reported.SuppressGraymail != nil || reported.LimitSendFrequency != nil {
		t.Errorf("reported settings = %+v, want none from an answer without a `to` object", reported)
	}
}

func TestSetSendListWithSettings_RefusesTheSameInputsAsSetSendList(t *testing.T) {
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no request expected on invalid input")
	})
	off := false
	settings := SendSettings{SuppressGraymail: &off}
	if _, _, err := c.SetSendListWithSettings(context.Background(), "", []string{"1"}, nil, settings); err == nil {
		t.Error("an empty email id must be refused")
	}
	if _, _, err := c.SetSendListWithSettings(context.Background(), "1", nil, nil, settings); err == nil {
		t.Error("an empty send list must be refused")
	}
	if _, _, err := c.SetSendListWithSettings(context.Background(), "1", []string{"7"}, []string{"7"}, settings); err == nil {
		t.Error("a send list that is also suppressed must be refused")
	}
}

func TestGetEmailSendSettings(t *testing.T) {
	cases := []struct {
		name              string
		body              string
		graymail, limited *bool
	}{
		{"both stated", `{"id":"555","to":{"suppressGraymail":false,"limitSendFrequency":true}}`, ptr(false), ptr(true)},
		{"one stated", `{"id":"555","to":{"suppressGraymail":true,"contactIlsLists":{"include":["1"],"exclude":[]}}}`, ptr(true), nil},
		{"a `to` that states neither", `{"id":"555","to":{}}`, nil, nil},
		{"no `to` object", `{"id":"555","name":"Template"}`, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured requestCapture
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				captured.record(r, false)
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := c.GetEmailSendSettings(context.Background(), " 555 ")
			if err != nil {
				t.Fatalf("GetEmailSendSettings: %v", err)
			}
			gotPath, gotMethod, _ := captured.seen(t)
			if gotMethod != http.MethodGet || gotPath != "/marketing/v3/emails/555" {
				t.Errorf("request = %s %s, want a GET of the email itself", gotMethod, gotPath)
			}
			if !sameBool(got.SuppressGraymail, tc.graymail) || !sameBool(got.LimitSendFrequency, tc.limited) {
				t.Errorf("settings = %s / %s, want %s / %s", show(got.SuppressGraymail), show(got.LimitSendFrequency), show(tc.graymail), show(tc.limited))
			}
		})
	}
}

func TestGetEmailSendSettings_Failures(t *testing.T) {
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no request expected for a blank id")
	})
	if _, err := c.GetEmailSendSettings(context.Background(), "  "); err == nil {
		t.Error("a blank email id must be refused")
	}

	gone, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := gone.GetEmailSendSettings(context.Background(), "555"); err == nil {
		t.Error("a missing email must be an error, not an email with no settings")
	}

	// A setting of the wrong type is a response this client cannot read, and says so.
	wrong, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"555","to":{"suppressGraymail":"no"}}`)
	})
	if _, err := wrong.GetEmailSendSettings(context.Background(), "555"); err == nil {
		t.Error("an unreadable setting must be an error, not a guess")
	}
}

func ptr(b bool) *bool { return &b }

func sameBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func show(b *bool) string {
	switch {
	case b == nil:
		return "unstated"
	case *b:
		return "on"
	default:
		return "off"
	}
}

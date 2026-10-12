// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetContactProperty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		switch r.URL.Path {
		case "/crm/v3/properties/contacts/country_dropdown":
			_, _ = io.WriteString(w, `{"name":"country_dropdown","label":"Country","type":"enumeration","fieldType":"select","options":[{"label":"South Korea","value":"South Korea"}]}`)
		case "/crm/v3/properties/contacts/nope":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"status":"error"}`)
		case "/crm/v3/properties/contacts/broken":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ctx := context.Background()

	p, err := c.GetContactProperty(ctx, "country_dropdown")
	if err != nil || p == nil {
		t.Fatalf("GetContactProperty: %v %v", p, err)
	}
	if !p.IsEnumeration() || len(p.Options) != 1 {
		t.Errorf("got %+v", p)
	}

	p, err = c.GetContactProperty(ctx, "nope")
	if err != nil || p != nil {
		t.Errorf("a missing property is (nil, nil), got %v %v", p, err)
	}

	if _, err = c.GetContactProperty(ctx, "broken"); err == nil {
		t.Error("a 2xx with no name must be an error, not an empty definition")
	}
	if _, err = c.GetContactProperty(ctx, "  "); err == nil {
		t.Error("a blank name must be refused before any request")
	}
}

func TestEventTypeProperties(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("includeProperties") != "true" {
			t.Errorf("properties must be requested")
		}
		if r.URL.Query().Get("after") == "" {
			_, _ = io.WriteString(w, `{"results":[{"objectTypeId":"6-48984571","properties":[{"name":"event_name"}]}],"paging":{"next":{"after":"p2"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"objectTypeId":"6-58204655","properties":[{"name":"course_name","label":"Course name","type":"string"}]}]}`)
	})
	got, err := c.EventTypeProperties(context.Background(), "6-58204655")
	if err != nil || len(got) != 1 || got[0].Name != "course_name" {
		t.Fatalf("got %+v %v", got, err)
	}
	if calls != 2 {
		t.Errorf("must page to find the definition, made %d calls", calls)
	}
	missing, err := c.EventTypeProperties(context.Background(), "6-1")
	if err != nil || missing != nil {
		t.Errorf("an absent definition is (nil, nil), got %v %v", missing, err)
	}
}

func TestValidationMessage_OnlyForValidationErrors(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if r.URL.Query().Get("kind") == "other" {
			_, _ = io.WriteString(w, `{"category":"BAD_REQUEST","message":"secret-ish detail"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"error","category":"VALIDATION_ERROR","message":"Filter branch of type UNIFIED_EVENTS\ncannot have filter branches"}`)
	})
	_, err := c.doRequest(context.Background(), http.MethodGet, "/crm/v3/lists/x", nil, true)
	if got := ValidationMessage(err); got != "Filter branch of type UNIFIED_EVENTS cannot have filter branches" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(err.Error(), "UNIFIED_EVENTS") {
		t.Error("Error() must never echo the body")
	}
	_, err = c.doRequest(context.Background(), http.MethodGet, "/crm/v3/lists/x?kind=other", nil, true)
	if got := ValidationMessage(err); got != "" {
		t.Errorf("a non-validation body must not be retained, got %q", got)
	}
}

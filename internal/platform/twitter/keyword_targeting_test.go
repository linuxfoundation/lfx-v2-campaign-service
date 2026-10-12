// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type tcReply struct {
	status int
	body   string
}

// targetingServer answers requests from a queue (the last entry repeats), recording each.
func targetingServer(t *testing.T, replies ...tcReply) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		reply := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(srv.Close)
	return testClient(srv.URL), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestListLineItemTargetingCriteria_ReadsEveryPageScopedToTheLineItem(t *testing.T) {
	c, seen := targetingServer(t,
		tcReply{http.StatusOK, `{"data":[{"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"kubernetes","operator_type":"EQ"}],"next_cursor":"c2"}`},
		tcReply{http.StatusOK, `{"data":[{"id":"k2","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"x","operator_type":"EQ"},{"id":"k3","line_item_id":"li1","targeting_type":"EXACT_KEYWORD","targeting_value":"ebpf","operator_type":"NE"}],"next_cursor":null}`},
	)
	got, err := c.ListLineItemTargetingCriteria(context.Background(), "li1")
	if err != nil {
		t.Fatalf("ListLineItemTargetingCriteria: %v", err)
	}
	if len(got) != 3 || !got[0].PositiveKeyword() || got[1].PositiveKeyword() || got[2].PositiveKeyword() {
		t.Fatalf("unexpected criteria: %+v", got)
	}
	reqs := seen()
	if len(reqs) != 2 || !strings.HasPrefix(reqs[0], "GET /12/accounts/account123/targeting_criteria?") ||
		!strings.Contains(reqs[0], "line_item_ids=li1") || !strings.Contains(reqs[1], "cursor=c2") {
		t.Errorf("requests = %v", reqs)
	}
}

func TestListLineItemTargetingCriteria_IsAllOrError(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no result set", `{"data":null,"next_cursor":null}`},
		{"criterion of another line item", `{"data":[{"id":"k1","line_item_id":"li9","targeting_type":"BROAD_KEYWORD","targeting_value":"a","operator_type":"EQ"}],"next_cursor":null}`},
		{"criterion without an id", `{"data":[{"line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"a","operator_type":"EQ"}],"next_cursor":null}`},
		{"not a list", `{"data":{"id":"k1"},"next_cursor":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := targetingServer(t, tcReply{http.StatusOK, tc.body})
			if _, err := c.ListLineItemTargetingCriteria(context.Background(), "li1"); !errors.Is(err, ErrTargetingUnreadable) {
				t.Fatalf("want ErrTargetingUnreadable, got %v", err)
			}
		})
	}
	t.Run("an id that cannot address a path never leaves the client", func(t *testing.T) {
		c, seen := targetingServer(t, tcReply{http.StatusOK, `{}`})
		if _, err := c.ListLineItemTargetingCriteria(context.Background(), "li/1"); !errors.Is(err, ErrInvalidLineItemID) || len(seen()) != 0 {
			t.Fatalf("want ErrInvalidLineItemID with zero requests, got %v", err)
		}
	})
}

func TestDeleteTargetingCriterion_Classification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reply       tcReply
		ok          bool
		notFound    bool
		unconfirmed bool
	}{
		{"deleted", tcReply{http.StatusOK, `{"data":{"id":"k1","deleted":true}}`}, true, false, false},
		{"404", tcReply{http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND"}]}`}, false, true, false},
		{"definite 400", tcReply{http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`}, false, false, false},
		{"5xx", tcReply{http.StatusServiceUnavailable, `{}`}, false, false, true},
		{"429 is never retried and unconfirmed", tcReply{http.StatusTooManyRequests, `{}`}, false, false, true},
		{"2xx without deleted=true", tcReply{http.StatusOK, `{"data":{"id":"k1","deleted":false}}`}, false, false, true},
		{"2xx naming another criterion", tcReply{http.StatusOK, `{"data":{"id":"k2","deleted":true}}`}, false, false, true},
		{"2xx without data", tcReply{http.StatusOK, `{}`}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := targetingServer(t, tc.reply)
			err := c.DeleteTargetingCriterion(context.Background(), "k1")
			if tc.ok {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
			} else if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, ErrTargetingCriterionNotFound) != tc.notFound {
				t.Errorf("not-found = %v, want %v (%v)", errors.Is(err, ErrTargetingCriterionNotFound), tc.notFound, err)
			}
			if err != nil && IsOutcomeUnconfirmed(err) != tc.unconfirmed {
				t.Errorf("unconfirmed = %v, want %v (%v)", IsOutcomeUnconfirmed(err), tc.unconfirmed, err)
			}
			if reqs := seen(); len(reqs) != 1 || !strings.HasPrefix(reqs[0], "DELETE /12/accounts/account123/targeting_criteria/k1") {
				t.Errorf("want exactly one DELETE, got %v", reqs)
			}
		})
	}
	t.Run("a malformed id never leaves the client", func(t *testing.T) {
		c, seen := targetingServer(t, tcReply{http.StatusOK, `{}`})
		if err := c.DeleteTargetingCriterion(context.Background(), "k1/../x"); !errors.Is(err, ErrInvalidTargetingCriterionID) || len(seen()) != 0 {
			t.Fatalf("want ErrInvalidTargetingCriterionID with zero requests, got %v", err)
		}
	})
	t.Run("a cancelled context is not sent", func(t *testing.T) {
		c, seen := targetingServer(t, tcReply{http.StatusOK, `{}`})
		c.writeDelay = 1 << 40 // force the pacer to wait, so the cancellation is observed there
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := c.DeleteTargetingCriterion(ctx, "k1")
		if err == nil || len(seen()) != 0 {
			t.Fatalf("want a refusal with zero requests, got %v after %d", err, len(seen()))
		}
		if IsOutcomeUnconfirmed(err) || !errors.Is(err, ErrWriteNotSent) {
			t.Errorf("an unsent request is ErrWriteNotSent, never unconfirmed: %v", err)
		}
	})
	// A connect-time failure proves the DELETE never left the process: NOT_SENT, not a refusal
	// (PR #274 review).
	t.Run("a refused connection is not sent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		base := srv.URL
		srv.Close() // nothing listens there now: the dial is refused
		c := testClient(base)
		err := c.DeleteTargetingCriterion(context.Background(), "k1")
		if !errors.Is(err, ErrWriteNotSent) || IsOutcomeUnconfirmed(err) {
			t.Fatalf("want ErrWriteNotSent and not unconfirmed, got %v", err)
		}
	})
}

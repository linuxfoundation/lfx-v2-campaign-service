// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package accesscheck

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

const testUID = "7cad5a8d-19d0-41a4-81a6-043453daf9ee"

// fakeRequester answers each subject from a table and records what was sent. A subject in
// checkSeq is answered from that sequence instead, one entry per request, so a test can script
// the second answer to a re-check.
type fakeRequester struct {
	replies  map[string]string
	errs     map[string]error
	checkSeq []fakeReply
	sent     map[string]string
	count    map[string]int
}

type fakeReply struct {
	data string
	err  error
}

func (f *fakeRequester) RequestWithContext(_ context.Context, subject string, data []byte) (*nats.Msg, error) {
	if f.sent == nil {
		f.sent = map[string]string{}
		f.count = map[string]int{}
	}
	f.sent[subject] = string(data)
	n := f.count[subject]
	f.count[subject]++
	if subject == AccessCheckSubject && f.checkSeq != nil {
		if n >= len(f.checkSeq) {
			return nil, errors.New("unexpected extra access check request")
		}
		if err := f.checkSeq[n].err; err != nil {
			return nil, err
		}
		return &nats.Msg{Subject: subject, Data: []byte(f.checkSeq[n].data)}, nil
	}
	if err := f.errs[subject]; err != nil {
		return nil, err
	}
	return &nats.Msg{Subject: subject, Data: []byte(f.replies[subject])}, nil
}

const wantToken = "project:" + testUID + "#global_marketing_ops@user:ada"

func TestIsGlobalMarketingOps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		slugReply string
		slugErr   error
		checkRepl string
		checkErr  error
		user      string
		want      bool
		wantErr   bool
	}{
		{name: "granted", slugReply: testUID, checkRepl: wantToken + "\ttrue", user: "ada", want: true},
		{name: "denied", slugReply: testUID, checkRepl: wantToken + "\tfalse", user: "ada"},
		{name: "granted among other lines, out of order", slugReply: testUID + "\n",
			checkRepl: "project:x#writer@user:ada\tfalse\n" + wantToken + "\ttrue\n", user: "ada", want: true},
		{name: "a line for another token is not an answer", slugReply: testUID,
			checkRepl: "project:" + testUID + "#global_marketing_ops@user:adam\ttrue", user: "ada", wantErr: true},
		{name: "fga-sync failure sentence", slugReply: testUID, checkRepl: "failed to check relationship", user: "ada", wantErr: true},
		{name: "unrecognised verdict", slugReply: testUID, checkRepl: wantToken + "\tmaybe", user: "ada", wantErr: true},
		{name: "access check transport error", slugReply: testUID, checkErr: nats.ErrNoResponders, user: "ada", wantErr: true},
		{name: "project not found", slugReply: `{"code":"not_found","message":"project not found"}`, user: "ada", wantErr: true},
		{name: "empty slug reply", slugReply: "", user: "ada", wantErr: true},
		{name: "slug transport error", slugErr: nats.ErrTimeout, user: "ada", wantErr: true},
		{name: "username with a newline", slugReply: testUID, checkRepl: wantToken + "\ttrue", user: "ada\nproject:x#global_marketing_ops@user:root", wantErr: true},
		{name: "username with a separator", slugReply: testUID, checkRepl: wantToken + "\ttrue", user: "ada#writer", wantErr: true},
		{name: "empty username", slugReply: testUID, checkRepl: wantToken + "\ttrue", user: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRequester{
				replies: map[string]string{SlugToUIDSubject: tc.slugReply, AccessCheckSubject: tc.checkRepl},
				errs:    map[string]error{SlugToUIDSubject: tc.slugErr, AccessCheckSubject: tc.checkErr},
			}
			got, err := New(r).IsGlobalMarketingOps(context.Background(), "cncf", tc.user)
			if tc.wantErr {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err = %v, want ErrUnavailable", err)
				}
				if got {
					t.Fatal("an error must never come with a grant")
				}
				// A username that could add or alter a check line is refused before
				// anything is sent, not merely failed to match afterwards.
				if !validUsername(tc.user) {
					if body, sent := r.sent[AccessCheckSubject]; sent {
						t.Fatalf("sent %q for an uncheckable username", body)
					}
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("IsGlobalMarketingOps = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			if r.sent[SlugToUIDSubject] != "cncf" {
				t.Fatalf("slug request body = %q, want the bare slug", r.sent[SlugToUIDSubject])
			}
			if r.sent[AccessCheckSubject] != wantToken {
				t.Fatalf("access check body = %q, want %q", r.sent[AccessCheckSubject], wantToken)
			}
		})
	}
}

func TestIsGlobalMarketingOps_NilRequester(t *testing.T) {
	if _, err := New(nil).IsGlobalMarketingOps(context.Background(), "cncf", "ada"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// fga-sync answers false, uncached, for an item whose OpenFGA check errored, so a first false is
// re-checked once and only the second answer counts. A grant is never re-checked.
func TestIsGlobalMarketingOps_RechecksAFalse(t *testing.T) {
	grant := fakeReply{data: wantToken + "\ttrue"}
	deny := fakeReply{data: wantToken + "\tfalse"}
	for _, tc := range []struct {
		name      string
		seq       []fakeReply
		want      bool
		wantErr   bool
		wantCheck int
	}{
		{name: "grant is not re-checked", seq: []fakeReply{grant}, want: true, wantCheck: 1},
		{name: "false then true is a grant", seq: []fakeReply{deny, grant}, want: true, wantCheck: 2},
		{name: "false then false is a denial", seq: []fakeReply{deny, deny}, wantCheck: 2},
		{name: "false then transport error is unavailable", seq: []fakeReply{deny, {err: nats.ErrTimeout}}, wantErr: true, wantCheck: 2},
		{name: "false then failure sentence is unavailable", seq: []fakeReply{deny, {data: "failed to check relationship"}}, wantErr: true, wantCheck: 2},
		{name: "an error is not re-checked", seq: []fakeReply{{err: nats.ErrTimeout}}, wantErr: true, wantCheck: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRequester{replies: map[string]string{SlugToUIDSubject: testUID}, checkSeq: tc.seq}
			got, err := New(r).IsGlobalMarketingOps(context.Background(), "cncf", "ada")
			if tc.wantErr {
				if !errors.Is(err, ErrUnavailable) || got {
					t.Fatalf("IsGlobalMarketingOps = (%v, %v), want (false, ErrUnavailable)", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("IsGlobalMarketingOps = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			if n := r.count[AccessCheckSubject]; n != tc.wantCheck {
				t.Fatalf("access check requests = %d, want %d", n, tc.wantCheck)
			}
			if n := r.count[SlugToUIDSubject]; n != 1 {
				t.Fatalf("slug lookups = %d, want 1 (the re-check reuses the UID)", n)
			}
		})
	}
}

// ProjectUID is the index relay's resolver (#590): a UID reply is returned, and a not-found
// RPCError, a non-UUID body, a transport error or a missing connection is ErrUnavailable.
func TestProjectUID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		err     error
		wantUID string
	}{
		{"uid", " " + testUID + "\n", nil, testUID},
		{"not found", `{"code":"not_found","message":"project not found"}`, nil, ""},
		{"empty", "", nil, ""},
		{"transport", "", errors.New("no responders"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRequester{replies: map[string]string{SlugToUIDSubject: tc.reply}, errs: map[string]error{SlugToUIDSubject: tc.err}}
			uid, err := New(r).ProjectUID(context.Background(), "cncf")
			if tc.wantUID != "" {
				if err != nil || uid != tc.wantUID {
					t.Fatalf("ProjectUID = %q, %v; want %q", uid, err, tc.wantUID)
				}
				if r.sent[SlugToUIDSubject] != "cncf" {
					t.Errorf("sent %q, want the slug", r.sent[SlugToUIDSubject])
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
		})
	}
	if _, err := New(nil).ProjectUID(context.Background(), "cncf"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil requester err = %v, want ErrUnavailable", err)
	}
}

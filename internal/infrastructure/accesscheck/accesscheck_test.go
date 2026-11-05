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

// fakeRequester answers each subject from a table and records what was sent.
type fakeRequester struct {
	replies map[string]string
	errs    map[string]error
	sent    map[string]string
}

func (f *fakeRequester) RequestWithContext(_ context.Context, subject string, data []byte) (*nats.Msg, error) {
	if f.sent == nil {
		f.sent = map[string]string{}
	}
	f.sent[subject] = string(data)
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

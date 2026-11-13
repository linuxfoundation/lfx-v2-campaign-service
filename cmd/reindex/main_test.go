// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

func TestParseArgs(t *testing.T) {
	var stderr bytes.Buffer
	o, err := parseArgs(nil, &stderr)
	if err != nil || o.apply || o.kind != "all" || o.batch != defaultBatch || o.project != "" {
		t.Fatalf("defaults = %+v, %v; want a dry run of everything", o, err)
	}
	o, err = parseArgs([]string{"-project", " tlf ", "-kind", "briefs", "-batch", "5", "-apply"}, &stderr)
	if err != nil || !o.apply || o.kind != "briefs" || o.batch != 5 || o.project != "tlf" {
		t.Fatalf("parsed = %+v, %v", o, err)
	}
	for _, bad := range [][]string{{"-kind", "jobs"}, {"-batch", "0"}, {"-batch", "1001"}, {"extra"}} {
		if _, err := parseArgs(bad, &stderr); err == nil {
			t.Errorf("parseArgs(%q) accepted", bad)
		}
	}
}

// fakeStore pages through fixed ids and records what the command asked for.
type fakeStore struct {
	briefIDs, campaignIDs []string
	applies               []bool
	projects              []string
	briefPayload          []byte
}

func page(ids []string, after string, limit int) postgres.ReindexBatch {
	var b postgres.ReindexBatch
	for _, id := range ids {
		if id <= after || b.Read == limit {
			continue
		}
		b.Read++
		b.Live++
		b.LastID = id
	}
	return b
}

func (f *fakeStore) ReindexBriefs(_ context.Context, project, after string, limit int, apply bool, build postgres.BriefPayloadFunc) (postgres.ReindexBatch, error) {
	f.applies = append(f.applies, apply)
	f.projects = append(f.projects, project)
	p, err := build("updated", &model.CampaignBrief{ID: "b1", ProjectID: "tlf", EventSlug: "kubecon"})
	if err != nil {
		return postgres.ReindexBatch{}, err
	}
	f.briefPayload = p
	return page(f.briefIDs, after, limit), nil
}

func (f *fakeStore) ReindexCampaigns(_ context.Context, project, after string, limit int, apply bool, _ postgres.CampaignPayloadFunc) (postgres.ReindexBatch, error) {
	f.applies = append(f.applies, apply)
	f.projects = append(f.projects, project)
	return page(f.campaignIDs, after, limit), nil
}

// The command pages until a short batch, passes -apply and -project through, totals the
// counts, and builds each payload with the service's own builder.
func TestRunPagesAndTotals(t *testing.T) {
	f := &fakeStore{briefIDs: []string{"a", "b", "c", "d", "e"}, campaignIDs: []string{"x", "y"}}
	var out bytes.Buffer
	if err := run(context.Background(), options{kind: "all", batch: 2, project: "tlf"}, f, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"DRY RUN", "project tlf", "briefs: read 5, live 5, deleted 0, skipped 0 (3 batches)", "campaigns: read 2, live 2, deleted 0, skipped 0 (2 batches)"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q lacks %q", got, want)
		}
	}
	for i, a := range f.applies {
		if a || f.projects[i] != "tlf" {
			t.Fatalf("call %d: apply=%v project=%q; a dry run of tlf must pass apply=false", i, a, f.projects[i])
		}
	}
	want, _ := service.BriefIndexPayload("updated", &model.CampaignBrief{ID: "b1", ProjectID: "tlf", EventSlug: "kubecon"})
	if !bytes.Equal(f.briefPayload, want) {
		t.Errorf("brief payload differs from service.BriefIndexPayload:\n%s\n%s", f.briefPayload, want)
	}
}

func TestRunKindAndApply(t *testing.T) {
	f := &fakeStore{campaignIDs: []string{"x"}}
	var out bytes.Buffer
	if err := run(context.Background(), options{kind: "campaigns", batch: 10, apply: true}, f, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "briefs:") || !strings.Contains(out.String(), "APPLY") {
		t.Errorf("output %q: -kind campaigns -apply must touch only campaigns, in apply mode", out.String())
	}
	if len(f.applies) != 1 || !f.applies[0] {
		t.Errorf("applies = %v, want one apply=true call", f.applies)
	}
}

// A store whose cursor never advances is stopped, not looped on.
type stuckStore struct{ *fakeStore }

func (stuckStore) ReindexBriefs(context.Context, string, string, int, bool, postgres.BriefPayloadFunc) (postgres.ReindexBatch, error) {
	return postgres.ReindexBatch{Read: 2, Live: 2, LastID: "a"}, nil
}

func TestRunStopsOnACursorThatDoesNotAdvance(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), options{kind: "briefs", batch: 2}, stuckStore{&fakeStore{}}, &out)
	if err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("err = %v, want the cursor guard", err)
	}
}

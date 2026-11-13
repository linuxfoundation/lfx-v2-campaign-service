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
	o, err = parseArgs([]string{"-project", " tlf ", "-kind", "briefs", "-batch", "5", "-apply", "-relay-ready"}, &stderr)
	if err != nil || !o.apply || !o.relayReady || o.kind != "briefs" || o.batch != 5 || o.project != "tlf" {
		t.Fatalf("parsed = %+v, %v", o, err)
	}
	// -apply without -relay-ready is refused: the command cannot see whether the target
	// deployment's relay would deliver the rows, or key them on the project UID.
	if _, err := parseArgs([]string{"-apply"}, &stderr); err == nil || !strings.Contains(err.Error(), "-relay-ready") {
		t.Errorf("-apply alone: err = %v, want the -relay-ready refusal", err)
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
	// payloads records what the command's builders produced, keyed "<kind>/<action>", so a
	// test can compare them with the service's own builders for both actions.
	payloads map[string][]byte
}

var (
	fixtureBrief    = &model.CampaignBrief{ID: "b1", ProjectID: "tlf", EventSlug: "kubecon"}
	fixtureCampaign = &model.Campaign{ID: "c1", ProjectID: "tlf", BriefID: "b1", Platform: model.ProviderGoogleAds, Status: "created", PlatformCampaignID: "123"}
)

func (f *fakeStore) record(key string, p []byte) {
	if f.payloads == nil {
		f.payloads = map[string][]byte{}
	}
	f.payloads[key] = p
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
	for _, action := range []string{"updated", "deleted"} {
		p, err := build(action, fixtureBrief)
		if err != nil {
			return postgres.ReindexBatch{}, err
		}
		f.record("brief/"+action, p)
	}
	return page(f.briefIDs, after, limit), nil
}

func (f *fakeStore) ReindexCampaigns(_ context.Context, project, after string, limit int, apply bool, build postgres.CampaignPayloadFunc) (postgres.ReindexBatch, error) {
	f.applies = append(f.applies, apply)
	f.projects = append(f.projects, project)
	for _, action := range []string{"updated", "deleted"} {
		p, err := build(action, fixtureCampaign)
		if err != nil {
			return postgres.ReindexBatch{}, err
		}
		f.record("campaign/"+action, p)
	}
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
	// Both builders, both actions, byte for byte against the service's own builders: the
	// command's guarantee is that a re-sent message is exactly what a write would produce.
	for _, action := range []string{"updated", "deleted"} {
		wantBrief, _ := service.BriefIndexPayload(action, fixtureBrief)
		if got := f.payloads["brief/"+action]; len(got) == 0 || !bytes.Equal(got, wantBrief) {
			t.Errorf("brief %s payload differs from service.BriefIndexPayload:\n%s\n%s", action, got, wantBrief)
		}
		wantCampaign, _ := service.CampaignIndexPayload(action, fixtureCampaign)
		if got := f.payloads["campaign/"+action]; len(got) == 0 || !bytes.Equal(got, wantCampaign) {
			t.Errorf("campaign %s payload differs from service.CampaignIndexPayload:\n%s\n%s", action, got, wantCampaign)
		}
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

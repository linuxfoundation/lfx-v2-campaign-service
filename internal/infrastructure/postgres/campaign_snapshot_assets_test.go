// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// ids returns the extracted set as a sorted slice, so a failure prints what was found rather
// than a map in whatever order the runtime chose.
func ids(t *testing.T, snapshot string) []string {
	t.Helper()
	got, err := snapshotAssetIDs([]byte(snapshot))
	if err != nil {
		t.Fatalf("snapshotAssetIDs(%s): %v", snapshot, err)
	}
	out := make([]string, 0, len(got))
	for id := range got {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func wantIDs(t *testing.T, snapshot string, want ...string) {
	t.Helper()
	got := ids(t, snapshot)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("snapshotAssetIDs(%s) = %v, want %v", snapshot, got, want)
	}
}

const (
	assetA = "11111111-1111-4111-8111-111111111111"
	assetB = "22222222-2222-4222-8222-222222222222"
)

// TestSnapshotAssetIDs_ExtractsEverySpelling pins the property the whole check rests on: the
// KEY decides, and it decides case-insensitively.
//
// config_snapshot is the caller's own JSON (service/brief.go stores
// redactedConfigSnapshot(p.Campaign.Config), a re-marshalled map[string]any), and every
// dispatcher binds that JSON with encoding/json, whose field matching is case-insensitive. So
// all of the spellings below name a real asset to the dispatcher that resolves it, and an
// extractor that recognised only one would silently let the others through — the same class of
// silent miss as a lock predicate that matches no row.
func TestSnapshotAssetIDs_ExtractsEverySpelling(t *testing.T) {
	// camelCase: the tag on dispatch/reddit.go, twitter.go and microsoft.go.
	wantIDs(t, `{"imageAssetId":"`+assetA+`"}`, assetA)
	// PascalCase: meta.AdVariant and linkedin.CreativeVariant carry NO json tag, so this is
	// the spelling a snapshot built from those types round-trips with.
	wantIDs(t, `{"ImageAssetID":"`+assetA+`"}`, assetA)
	// Neither, but still bound by encoding/json, so still a live reference.
	wantIDs(t, `{"IMAGEASSETID":"`+assetA+`"}`, assetA)
	wantIDs(t, `{"imageassetid":"`+assetA+`"}`, assetA)
}

// TestSnapshotAssetIDs_WalksNestedShapes pins that the walk reaches the place the two biggest
// platforms actually put the id: inside `variants`, an ARRAY of objects. A top-level-only scan
// would find nothing at all for Meta and LinkedIn.
func TestSnapshotAssetIDs_WalksNestedShapes(t *testing.T) {
	wantIDs(t, `{"variants":[{"ImageAssetID":"`+assetA+`"},{"ImageAssetID":"`+assetB+`"}]}`, assetA, assetB)
	wantIDs(t, `{"a":{"b":[[{"imageAssetId":"`+assetA+`"}]]}}`, assetA)
	// The same asset named twice is one id, not two: the result is a set.
	wantIDs(t, `{"imageAssetId":"`+assetA+`","variants":[{"ImageAssetID":"`+assetA+`"}]}`, assetA)
}

// TestSnapshotAssetIDs_CanonicalizesAliases pins that the four spellings uuid.Parse accepts all
// collapse to one id.
//
// This is what makes the "newly introduced" diff in checkNewSnapshotAssetRefs meaningful.
// Without it, re-sending an id the row already carries in a different spelling would read as a
// new reference, and the replace would be refused for an asset that was never added.
func TestSnapshotAssetIDs_CanonicalizesAliases(t *testing.T) {
	unhyphenated := strings.ReplaceAll(assetA, "-", "")
	wantIDs(t, `{"imageAssetId":"{`+assetA+`}"}`, assetA)
	wantIDs(t, `{"imageAssetId":"urn:uuid:`+assetA+`"}`, assetA)
	wantIDs(t, `{"imageAssetId":"`+unhyphenated+`"}`, assetA)
	wantIDs(t, `{"imageAssetId":"`+strings.ToUpper(assetA)+`"}`, assetA)
	// Surrounding whitespace is the caller's spelling problem, not a different asset — the
	// dispatchers TrimSpace before parsing too (dispatch/meta.go, reddit_asset.go).
	wantIDs(t, `{"imageAssetId":"  `+assetA+`  "}`, assetA)
}

// TestSnapshotAssetIDs_IgnoresUUIDsThatAreNotAssetReferences is the reason extraction is driven
// by the key rather than by the SHAPE of the value.
//
// A snapshot legitimately carries uuids that are not creative assets. Collecting every
// uuid-shaped token would send the brief's own id to the creative_assets lookup, find nothing,
// and refuse a perfectly good config for an asset nobody referenced.
func TestSnapshotAssetIDs_IgnoresUUIDsThatAreNotAssetReferences(t *testing.T) {
	wantIDs(t, `{"briefId":"`+assetB+`","audienceId":"`+assetB+`","imageAssetId":"`+assetA+`"}`, assetA)
	// A key that merely CONTAINS the asset key is a different field.
	wantIDs(t, `{"backupImageAssetId":"`+assetB+`"}`)
	wantIDs(t, `{"imageAssetIds":"`+assetB+`"}`)
}

// TestSnapshotAssetIDs_SkipsValuesThatAreNotIDs pins that a malformed or absent reference is
// passed over rather than turned into a lookup value.
//
// Both halves matter. creative_assets.id is a uuid column, so a non-uuid carried into
// presentCreativeAssetsQuery would raise a cast error instead of a refusal; and a dispatcher
// already refuses an unparseable asset id with a message that names the offending value, which
// is a better answer than this check could give.
func TestSnapshotAssetIDs_SkipsValuesThatAreNotIDs(t *testing.T) {
	wantIDs(t, `{"imageAssetId":""}`)
	wantIDs(t, `{"imageAssetId":"not-a-uuid"}`)
	wantIDs(t, `{"imageAssetId":null}`)
	wantIDs(t, `{"imageAssetId":42}`)
	wantIDs(t, `{"imageAssetId":{"nested":"`+assetA+`"}}`)
	wantIDs(t, `{"imageAssetId":["`+assetA+`"]}`)
}

// TestSnapshotAssetIDs_EmptySnapshotsNameNothing pins that the absent cases are ORDINARY, not
// errors. ReplaceCampaign runs this on every call, including the four callers that write back a
// snapshot they never looked at, so "no snapshot" has to be free and silent.
func TestSnapshotAssetIDs_EmptySnapshotsNameNothing(t *testing.T) {
	for _, snapshot := range []string{"", "   ", "null", "{}", "[]", `{"headline":"no asset yet"}`} {
		got, err := snapshotAssetIDs([]byte(snapshot))
		if err != nil {
			t.Errorf("snapshotAssetIDs(%q): %v", snapshot, err)
		}
		if len(got) != 0 {
			t.Errorf("snapshotAssetIDs(%q) = %v, want none", snapshot, got)
		}
	}
	if got, err := snapshotAssetIDs(nil); err != nil || got != nil {
		t.Errorf("snapshotAssetIDs(nil) = %v, %v; want nil, nil", got, err)
	}
}

// TestSnapshotAssetIDs_MalformedJSONIsReported pins that undecodable bytes surface as an error
// rather than as "this snapshot names no assets". Treating them as an empty set would turn a
// snapshot nothing can read into a silent pass straight through the very check that exists to
// catch a dangling reference.
func TestSnapshotAssetIDs_MalformedJSONIsReported(t *testing.T) {
	if _, err := snapshotAssetIDs([]byte(`{"imageAssetId":`)); err == nil {
		t.Fatal("malformed JSON decoded without error")
	}
}

// TestSnapshotAssetIDs_ReadsTheMetaAndLinkedInVariantsAsMarshalled is the drift guard for the
// one spelling nothing in this repo writes down.
//
// meta.AdVariant and linkedin.CreativeVariant declare ImageAssetID with no json tag, so the
// stored key is PascalCase purely as a consequence of Go's default. Adding `json:"imageAssetId"`
// to either type — a change that looks like tidying — would move the key, and only a test that
// marshals the REAL types notices. A literal in a fixture would not.
func TestSnapshotAssetIDs_ReadsTheMetaAndLinkedInVariantsAsMarshalled(t *testing.T) {
	for name, config := range map[string]any{
		"meta":     map[string]any{"variants": []meta.AdVariant{{ImageAssetID: assetA}}},
		"linkedin": map[string]any{"variants": []linkedin.CreativeVariant{{ImageAssetID: assetA}}},
	} {
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		got := ids(t, string(raw))
		if len(got) != 1 || got[0] != assetA {
			t.Errorf("%s: snapshotAssetIDs(%s) = %v, want [%s]", name, raw, got, assetA)
		}
	}
}

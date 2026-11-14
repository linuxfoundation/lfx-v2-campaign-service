// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// TestWithMetaUserPausedAd: the per-ad record (#550, gap 14) adds each id once in order, removes one
// on resume, drops the key when empty, folds case variants and keeps every other key.
func TestWithMetaUserPausedAd(t *testing.T) {
	base := json.RawMessage(`{"AccountID":"act_777","AdSetID":"888","UserPausedAdSetID":"888"}`)
	r, err := WithMetaUserPausedAd(base, "901", true)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = WithMetaUserPausedAd(r, "902", true)
	r, _ = WithMetaUserPausedAd(r, "901", true)
	if got := MetaUserPausedAdIDs(r); !reflect.DeepEqual(got, []string{"901", "902"}) {
		t.Fatalf("ids = %v, want [901 902]", got)
	}
	if !MetaAdHeldPaused(r, "902") || MetaAdHeldPaused(r, "903") || MetaAdHeldPaused(r, "") {
		t.Fatalf("held: 902 %v, 903 %v, empty %v", MetaAdHeldPaused(r, "902"), MetaAdHeldPaused(r, "903"), MetaAdHeldPaused(r, ""))
	}
	if MetaRecordedAdSetID(r) != "888" || MetaUserPausedAdSetID(r) != "888" {
		t.Fatalf("other keys lost: %s", r)
	}
	r, _ = WithMetaUserPausedAd(r, "901", false)
	if got := MetaUserPausedAdIDs(r); !reflect.DeepEqual(got, []string{"902"}) {
		t.Fatalf("after resume ids = %v, want [902]", got)
	}
	r, _ = WithMetaUserPausedAd(r, "902", false)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(r, &fields)
	if _, has := fields[MetaResultUserPausedAdsKey]; has {
		t.Fatalf("an empty record must drop the key: %s", r)
	}
	folded, _ := WithMetaUserPausedAd(json.RawMessage(`{"userpausedadids":["7"],"UserPausedAdIDs":["8"]}`), "9", true)
	if got := MetaUserPausedAdIDs(folded); len(got) != 3 || !MetaAdHeldPaused(folded, "7") || !MetaAdHeldPaused(folded, "8") {
		t.Fatalf("case variants not folded: %s", folded)
	}
	if empty, err := WithMetaUserPausedAd(nil, "5", true); err != nil || !MetaAdHeldPaused(empty, "5") {
		t.Fatalf("empty result: %s, %v", empty, err)
	}
	if _, err := WithMetaUserPausedAd(json.RawMessage(`[1]`), "5", true); !errors.Is(err, ErrMetaResultNotObject) {
		t.Fatalf("non-object err = %v", err)
	}
}

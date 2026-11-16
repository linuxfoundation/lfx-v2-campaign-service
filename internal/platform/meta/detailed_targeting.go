// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// graphID is a Graph object id that Meta renders either as a JSON string or as a JSON number (the
// Targeting Search and targeting validation answers use integers). It decodes both into the
// decimal string this package compares, and refuses anything else.
type graphID string

func (g *graphID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*g = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*g = graphID(strings.TrimSpace(s))
		return nil
	}
	if !numericIDRE.Match(b) {
		return errors.New("meta: an id that is neither a string nor a whole number")
	}
	*g = graphID(b)
	return nil
}

// targetingValidationRow is one row of the ad account targeting validation read (GET
// /act_<id>/targetingvalidation, Ad Account Targeting Validation reference: "data: A list of
// AdAccountTargetingUnified nodes"). The reference names the request's `targeting_list` (type and id
// pairs, type one of the targeting enum, behaviors and work_positions included) but not the row's
// fields; the targeting search page's validation example answers id, name and valid, so those are
// what is read. NOT live-verified.
type targetingValidationRow struct {
	ID    graphID `json:"id"`
	Type  string  `json:"type"`
	Valid *bool   `json:"valid"`
}

// detailedTargetingRef is one entry of the validation read's targeting_list.
type detailedTargetingRef struct {
	Type string      `json:"type"`
	ID   json.Number `json:"id"`
}

// checkDetailedTargeting proves, before the first mutating call, that every behavior and work
// position t names is one Meta currently offers to accountID, in ONE read:
// GET /act_<id>/targetingvalidation?targeting_list=[{"type":"behaviors","id":…},
// {"type":"work_positions","id":…}]. Like checkInterests: a read that fails, an answer without
// `data`, an id left unanswered or a row without a `valid` field is an error; a row with valid false
// is a refusal. Either way nothing is created, so a retired id cannot orphan a campaign at the ad
// set.
func (c *Client) checkDetailedTargeting(ctx context.Context, accountID string, t *AudienceTargeting) error {
	if t == nil || (len(t.Behaviors) == 0 && len(t.WorkPositions) == 0) {
		return nil
	}
	refs := make([]detailedTargetingRef, 0, len(t.Behaviors)+len(t.WorkPositions))
	kind := map[string]string{}
	for _, list := range []struct {
		typ, field string
		ids        []string
	}{{"behaviors", "behavior", t.Behaviors}, {"work_positions", "work position", t.WorkPositions}} {
		for _, id := range list.ids {
			refs = append(refs, detailedTargetingRef{Type: list.typ, ID: json.Number(id)})
			kind[list.typ+"/"+id] = list.field
		}
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("meta targeting: encode the targeting list: %w", err)
	}
	raw, err := c.getChecked(ctx, "/"+accountID+"/targetingvalidation?targeting_list="+url.QueryEscape(string(encoded)))
	if err != nil {
		return fmt.Errorf("meta targeting: the behaviors and work positions could not be validated, so nothing was created: %w", err)
	}
	var page struct {
		Data *[]targetingValidationRow `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.Data == nil {
		return errors.New("meta targeting: the targeting validation read returned no rows, so nothing was created")
	}
	// Replies are matched on type AND id: the same id may be listed as both a behavior and a work
	// position, and one reply must never answer for the other. A reply without a type answers only
	// for an id listed under a single type. Two replies for one (type, id) that disagree on
	// validity are refused rather than letting their order decide.
	type refKey struct{ typ, id string }
	rows := make(map[refKey]targetingValidationRow, len(*page.Data))
	for _, row := range *page.Data {
		k := refKey{strings.TrimSpace(row.Type), string(row.ID)}
		if prev, dup := rows[k]; dup && !sameValidity(prev.Valid, row.Valid) {
			return fmt.Errorf("meta targeting: the targeting validation read answered %s %s twice with different results, so nothing was created", k.typ, domain.CallerValue(k.id))
		}
		rows[k] = row
	}
	listed := map[string]int{}
	for _, ref := range refs {
		listed[string(ref.ID)]++
	}
	for _, ref := range refs {
		id := string(ref.ID)
		row, ok := rows[refKey{ref.Type, id}]
		if !ok && listed[id] == 1 {
			row, ok = rows[refKey{"", id}]
		}
		if !ok {
			return fmt.Errorf("meta targeting: the targeting validation read did not answer for %s %s, so nothing was created", kind[ref.Type+"/"+id], domain.CallerValue(id))
		}
		if row.Valid == nil {
			return fmt.Errorf("meta targeting: the targeting validation read did not say whether %s %s is valid, so nothing was created", kind[ref.Type+"/"+id], domain.CallerValue(id))
		}
		if !*row.Valid {
			return domain.PreflightRefusalf("meta targeting: %s %s is not a detailed targeting option Meta currently offers this ad account; nothing was created", kind[ref.Type+"/"+id], domain.CallerValue(id))
		}
	}
	return nil
}

// sameValidity reports whether two validation replies agree: both without a `valid` field, or both
// with the same value.
func sameValidity(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

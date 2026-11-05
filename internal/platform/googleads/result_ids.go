// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import "fmt"

// A repeated resource id in a CONFIRMED mutate result (#470 review).
//
// Every create-path mutate here checks that a 2xx answered with exactly one result per
// operation and that each result is a well-formed resource of the right kind, in this
// account and under the expected parent. That still accepts a response naming the SAME
// resource twice: n operations create n resources, so a result that repeats one leaves
// another operation unaccounted for, and recording it would persist one id where two
// resources may exist. resultIDSet is the one check every confirmed-result loop runs, and
// a repeat is UNCONFIRMED. Each loop keeps its own id contract on that path: a criteria
// loop returns no ids, an asset loop returns every valid id once (uniqueIDs).
//
// A CREATIVE asset is the one exception, and only for a repeat whose two operations sent the
// same content. Google returns the EXISTING asset for a duplicate asset create instead of a
// second one:
// https://ads-developers.googleblog.com/2022/03/changes-to-handling-duplicate-assets-in.html
// So one image used in two slots — a square marketing image that is also the square logo —
// comes back with the same id at both indexes, and no operation is unaccounted for: both
// asked for that one asset. resultIDSet.reuse accepts that repeat at every position it
// fills, so the ad or asset group still references the asset in each slot, and the loop
// reports the id once. A repeat whose operations sent DIFFERENT content is still UNCONFIRMED.
// Criteria and link loops keep repeat: a criterion or a link is never shared that way.

// resultIDSet records the ids a mutate response has named so far, by result index.
type resultIDSet map[string]int

func newResultIDSet(n int) resultIDSet { return make(resultIDSet, n) }

// repeat records id at result index i and returns "". When id was already recorded it
// returns the clause naming both indexes, for the caller's UNCONFIRMED error, and keeps
// the first record.
func (s resultIDSet) repeat(id string, i int) string {
	if j, ok := s[id]; ok {
		return fmt.Sprintf("2xx names resource id %s at index %d and again at index %d, so "+
			"one operation is unaccounted for", id, j, i)
	}
	s[id] = i
	return ""
}

// reuse is repeat for a CREATIVE-ASSET result loop. A new id is recorded at index i and
// returns first true. A repeat is accepted (first false, dup "") when same reports that the
// operation at the id's first index sent the same content as the operation at i — Google's
// answer to a duplicate asset create is the existing asset. Any other repeat returns repeat's
// clause, for the caller's UNCONFIRMED error.
func (s resultIDSet) reuse(id string, i int, same func(first int) bool) (first bool, dup string) {
	if j, ok := s[id]; ok && same(j) {
		return false, ""
	}
	if dup := s.repeat(id, i); dup != "" {
		return false, dup
	}
	return true, ""
}

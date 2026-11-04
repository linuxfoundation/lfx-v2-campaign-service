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

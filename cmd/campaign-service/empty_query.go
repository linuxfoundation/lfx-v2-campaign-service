// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
)

// refuseEmptyQueryParam makes an explicitly empty optional query parameter (`?q=`) reach the
// generated decoder as a blank value instead of as an omitted one.
//
// Goa's decoder for an optional string reads `qp.Get(name)` and treats "" as absent, so `?q=`
// decodes exactly like no q at all, and the design's MinLength(1)/Pattern(`\S`) never run. On
// list-google-ads-audiences (#496) that turns a malformed search into an unfiltered list. The
// generated code is not hand-edited; instead this wrapper rewrites the first value of name,
// when it is present and empty, to a single space. The decoder then sees a non-empty value,
// runs the design's Pattern, and refuses it with its ordinary 400, so the refusal keeps the
// standard error body and wording. Only the first value matters: it is the one Get returns.
func refuseEmptyQueryParam(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if vals, present := q[name]; present && len(vals) > 0 && vals[0] == "" {
			vals[0] = " "
			q[name] = vals
			r2 := r.Clone(r.Context())
			r2.URL.RawQuery = q.Encode()
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

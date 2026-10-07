// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"strings"

	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

// valueEchoingValidationMessages maps every Goa validation error whose default message formats
// the REJECTED VALUE into the response ("... but got value %q", "invalid value %#v for %q") to a
// fixed sentence that names the field and the kind of rule it broke, never the value. Each entry
// is completed with the field name as "<field> <sentence>". Errors not listed here — missing_field,
// missing_payload, decode_payload, unsupported_media_type, and every error a service method
// returns — keep Goa's default message.
var valueEchoingValidationMessages = map[string]string{
	goa.InvalidPattern:   "does not match the pattern this field requires",
	goa.InvalidLength:    "is outside the length this field allows",
	goa.InvalidRange:     "is outside the range this field allows",
	goa.InvalidEnumValue: "is not one of the values this field allows",
	goa.InvalidFormat:    "is not in the format this field requires",
	goa.InvalidFieldType: "is not of the type this field requires",
}

// nonEchoingErrorFormatter is the error formatter every generated HTTP server in this service is
// built with. It is goahttp.NewErrorResponse — same status, name, id and flags — except that a
// decoder validation error no longer carries the rejected value in its message (LFXV2-2665: the
// generated decoder applies a method's Pattern/MaxLength BEFORE the service method runs, so a
// method's own fixed, non-echoing 400 is never reached for those inputs, and Goa's default
// message would hand the raw input back). The field NAME is kept: it comes from the design, not
// the request. The rules themselves are published in the OpenAPI documents.
//
// A merged error (Goa joins every failed validation of one payload with "; ") is rewritten part
// by part from its history, so one echoing part cannot ride along with a rewritten one; a part
// that does not echo keeps only its own text (see the loop). A part without a field name is
// described as "a field". An error with no echoing part keeps its message untouched.
//
// Passing a non-nil formatter also keeps goahttp.ErrorEncoder from writing its shared default
// into the closure on every call.
func nonEchoingErrorFormatter(ctx context.Context, err error) goahttp.Statuser {
	resp := goahttp.NewErrorResponse(ctx, err)
	var gerr *goa.ServiceError
	if !errors.As(err, &gerr) {
		return resp
	}
	er, ok := resp.(*goahttp.ErrorResponse)
	if !ok {
		return resp
	}
	parts := gerr.History()
	msgs := make([]string, 0, len(parts))
	rewritten := false
	for _, part := range parts {
		sentence, echoes := valueEchoingValidationMessages[part.Name]
		if !echoes {
			// goa.MergeErrors mutates the accumulating error in place, and that same error is the
			// first entry of its own history, so its Message by now ends with "; " and every
			// LATER part's message — echoing ones included. Its own text is what precedes the
			// first separator.
			own, _, _ := strings.Cut(part.Message, "; ")
			msgs = append(msgs, own)
			continue
		}
		field := "a field"
		if part.Field != nil && *part.Field != "" {
			field = *part.Field
		}
		msgs = append(msgs, field+" "+sentence)
		rewritten = true
	}
	if rewritten {
		er.Message = strings.Join(msgs, "; ")
	}
	return er
}

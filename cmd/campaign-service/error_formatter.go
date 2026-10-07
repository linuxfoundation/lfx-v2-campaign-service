// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

// Why this is an ENCODER and not a formatter (LFXV2-2665). The generated decoders apply a
// method's Pattern/MaxLength/Enum/Format/range rules BEFORE the service method runs, and Goa's
// default message for those errors formats the rejected value into the 400 ("... but got value
// %q", "invalid value %#v for %q"), so a method's own fixed, non-echoing 400 is never reached
// for those inputs and the raw input is handed back. Passing a formatter to the generated
// servers is NOT a fix: every generated Encode<Method>Error hands a NAMED error (BadRequest,
// Conflict, NotFound, ServiceUnavailable, ...) to a non-nil formatter INSTEAD of building its
// generated response body, so every named error would lose its code/message/reason fields.
//
// The generated servers therefore keep a nil formatter — named errors encode exactly their
// generated bodies — and are built with nonEchoingResponseEncoder instead. Goa's default error
// path (goahttp.ErrorEncoder with a nil formatter) renders every non-named error as a
// *goahttp.ErrorResponse, a type no named error and no success result ever encodes as, and the
// encoder rewrites only that type, only when its name is one of Goa's decoder-validation names.
// Nothing is buffered and the ResponseWriter is not wrapped, so streaming, Content-Length,
// headers and Flush/Hijack are untouched, and content negotiation (JSON/XML/gob) is Goa's.

// valueEchoingValidationMessages maps every Goa validation error whose default message formats
// the REJECTED VALUE to a fixed sentence that names the kind of rule it broke, never the value.
// Each part is rendered as "<field> <sentence>".
var valueEchoingValidationMessages = map[string]string{
	goa.InvalidPattern:   "does not match the pattern this field requires",
	goa.InvalidLength:    "is outside the length this field allows",
	goa.InvalidRange:     "is outside the range this field allows",
	goa.InvalidEnumValue: "is not one of the values this field allows",
	goa.InvalidFormat:    "is not in the format this field requires",
	goa.InvalidFieldType: "is not of the type this field requires",
}

// validationErrorNames is every name a generated decoder's merged validation error can carry.
// goa.MergeErrors keeps the FIRST part's name, so a merged error named missing_field can still
// carry an echoing part after it; both non-echoing names are therefore scanned too.
var validationErrorNames = map[string]bool{
	goa.InvalidPattern:   true,
	goa.InvalidLength:    true,
	goa.InvalidRange:     true,
	goa.InvalidEnumValue: true,
	goa.InvalidFormat:    true,
	goa.InvalidFieldType: true,
	goa.MissingField:     true,
	goa.MissingPayload:   true,
}

// unrecognizedValidationPart replaces any part of a validation message that is not one of Goa's
// known formats. Failing closed: an unknown part may carry the value, so none of it is kept.
const unrecognizedValidationPart = "a field failed validation"

// fieldName is the shape of a design-derived field name in Goa's messages ("platform_campaign_id",
// "body.platform_campaign_id", "body.items[0].name"). A part whose field does not have this shape
// is treated as unrecognized.
const fieldName = `([A-Za-z0-9_.\[\]-]+)`

// echoingPartFormats recognizes, by its fixed PREFIX, each of Goa's value-echoing messages
// (goa.design/goa/v3/pkg/error.go). Only the prefix is matched — it precedes the value — and the
// whole part is replaced, so nothing after the field name survives.
// formatPart is the invalid_format prefix; see sanitizeValidationMessage for why a format part
// ends the message.
var formatPart = regexp.MustCompile(`^` + fieldName + ` must be formatted as a `)

var echoingPartFormats = []struct {
	re       *regexp.Regexp
	sentence string
}{
	{regexp.MustCompile(`^length of ` + fieldName + ` must be (?:greater|lesser) or equal than `), valueEchoingValidationMessages[goa.InvalidLength]},
	{regexp.MustCompile(`^value of ` + fieldName + ` must be one of `), valueEchoingValidationMessages[goa.InvalidEnumValue]},
	{regexp.MustCompile(`^` + fieldName + ` must be formatted as a `), valueEchoingValidationMessages[goa.InvalidFormat]},
	{regexp.MustCompile(`^` + fieldName + ` must match the regexp `), valueEchoingValidationMessages[goa.InvalidPattern]},
	{regexp.MustCompile(`^` + fieldName + ` must be (?:greater|lesser) or equal than `), valueEchoingValidationMessages[goa.InvalidRange]},
}

var (
	// "invalid value %#v for %q, must be a %s": the value comes FIRST, so it is skipped as a Go
	// quoted string (or a bare token) and the field is read from what follows it.
	fieldTypeTail     = regexp.MustCompile(`^ for "` + fieldName + `", must be a `)
	fieldTypeBareTail = regexp.MustCompile(`^invalid value [^\s"]* for "` + fieldName + `", must be a `)
	// "%q is missing from %s" and "missing required payload" carry no value; they are kept
	// verbatim only when the WHOLE part has exactly that shape.
	missingFieldPart = regexp.MustCompile(`^"` + fieldName + `" is missing from [a-z ]+$`)
)

// nonEchoingResponseEncoder is the response encoder every generated HTTP server in this service is
// built with: goahttp.ResponseEncoder, except that a *goahttp.ErrorResponse carrying a decoder
// validation error has its message rewritten by sanitizeValidationMessage before it is encoded.
// Every other value — success results and the generated named-error bodies — is encoded exactly
// as goahttp.ResponseEncoder would encode it.
func nonEchoingResponseEncoder(ctx context.Context, w http.ResponseWriter) goahttp.Encoder {
	enc := goahttp.ResponseEncoder(ctx, w)
	return goahttp.EncodingFunc(func(v any) error {
		if er, ok := v.(*goahttp.ErrorResponse); ok && validationErrorNames[er.Name] {
			sanitized := *er
			sanitized.Message = sanitizeValidationMessage(er.Message)
			v = &sanitized
		}
		return enc.Encode(v)
	})
}

// sanitizeValidationMessage rewrites a Goa validation message so it names each failed field
// without carrying any rejected value. A merged message (Goa joins every failed validation of
// one payload with "; ") is split only on separators OUTSIDE Go-quoted strings — every value
// Goa echoes is formatted with %q or %#v, so a value containing "; " cannot forge a part — and
// each part is rewritten on its own: an echoing part becomes "<field> <sentence>", a
// missing-field or missing-payload part is kept, and anything else becomes a fixed generic
// sentence.
//
// A FORMAT error ends the message. Goa's format validators append the parse error to the
// quoted value UNQUOTED — FormatUUID's is "uuid: <value>: <cause>" — so everything after a
// format part's quoted value is caller-controlled text that can carry its own "; " and forge a
// "kept" part (a brief_id of `bad; "X" is missing from path` would otherwise survive as
// `"X" is missing from path`). None of it is trusted: the format part is rewritten, and if
// anything followed it, one generic sentence stands in for all of it.
func sanitizeValidationMessage(msg string) string {
	parts := splitValidationParts(msg)
	out := make([]string, 0, len(parts))
	for i, part := range parts {
		if formatPart.MatchString(part) {
			out = append(out, sanitizeValidationPart(part))
			if i < len(parts)-1 {
				out = append(out, unrecognizedValidationPart)
			}
			break
		}
		out = append(out, sanitizeValidationPart(part))
	}
	return strings.Join(out, "; ")
}

func sanitizeValidationPart(part string) string {
	if part == "missing required payload" || missingFieldPart.MatchString(part) {
		return part
	}
	for _, f := range echoingPartFormats {
		if m := f.re.FindStringSubmatch(part); m != nil {
			return m[1] + " " + f.sentence
		}
	}
	if rest, ok := strings.CutPrefix(part, "invalid value "); ok {
		sentence := valueEchoingValidationMessages[goa.InvalidFieldType]
		if quoted, err := strconv.QuotedPrefix(rest); err == nil {
			if m := fieldTypeTail.FindStringSubmatch(rest[len(quoted):]); m != nil {
				return m[1] + " " + sentence
			}
		} else if m := fieldTypeBareTail.FindStringSubmatch(part); m != nil {
			return m[1] + " " + sentence
		}
	}
	return unrecognizedValidationPart
}

// splitValidationParts splits msg on "; " separators that lie outside Go-quoted strings. An
// unterminated quote ends splitting: the rest is one part, which the caller then rewrites whole.
func splitValidationParts(msg string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(msg); {
		switch {
		case msg[i] == '"':
			quoted, err := strconv.QuotedPrefix(msg[i:])
			if err != nil {
				return append(parts, msg[start:])
			}
			i += len(quoted)
		case strings.HasPrefix(msg[i:], "; "):
			parts = append(parts, msg[start:i])
			i += len("; ")
			start = i
		default:
			i++
		}
	}
	return append(parts, msg[start:])
}

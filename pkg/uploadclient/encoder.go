// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package uploadclient holds the request encoder a Go caller passes to the generated briefs
// client so that a creative-asset upload DECLARES its Content-Length (issue #183).
//
// # Why the generated client needs a different encoder
//
// goa's goahttp.RequestEncoder marshals the payload into a reader it installs as req.Body, but
// it never sets req.ContentLength. BuildUploadCreativeAssetRequest builds the request with a nil
// body, so ContentLength stays 0, and net/http treats a non-nil body with ContentLength 0 as
// "length unknown" and sends it chunked. The server then sees ContentLength -1.
//
// That matters on exactly one route. middleware.UploadAdmission prices the creative-asset upload
// from its declared size (constants.UploadAdmissionWeightFor), and an undeclared body is charged
// the worst-case ceiling, which is the whole admission budget. So with goa's stock encoder every
// upload from the generated client, however small, takes the entire budget and uploads run one
// at a time. That server rule is deliberate and stays: the permit is taken before the body is
// read, and an undeclared body may be worst-case. The fix is for the client to declare.
//
// RequestEncoder is a drop-in for goahttp.RequestEncoder: same Content-Type default, same JSON
// bytes (encoding/json, as goa uses), plus ContentLength and a replayable GetBody. It is safe on
// every endpoint of every generated client, not only the upload; it is NEEDED on the upload.
//
//	c := briefsc.NewClient("https", host, doer, uploadclient.RequestEncoder,
//		goahttp.ResponseDecoder, false)
package uploadclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	goahttp "goa.design/goa/v3/http"
)

// RequestEncoder returns an encoder for r that, on Encode, installs the JSON body and publishes
// its encoded length in r.ContentLength. Pass it as the enc argument of a generated NewClient.
func RequestEncoder(r *http.Request) goahttp.Encoder {
	const k = "Content-Type"
	if r.Header.Get(k) == "" {
		r.Header.Set(k, "application/json")
	}
	return &lengthEncoder{req: r}
}

// lengthEncoder marshals first and only then touches the request, so the body, its length and
// GetBody are always set together from the same bytes.
type lengthEncoder struct {
	req *http.Request
}

// Encode marshals v and installs it as the request body with a declared length.
//
// The length is the load-bearing line: without it net/http sends the body chunked and the
// server charges the upload the whole admission budget. TestUploadClient_DeclaredLengthUploadsRunConcurrently
// in cmd/campaign-service fails if it is removed.
func (e *lengthEncoder) Encode(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.req.ContentLength = int64(len(b))
	e.req.Body = io.NopCloser(bytes.NewReader(b))
	// GetBody lets the transport replay the body (redirects, HTTP/2 GOAWAY retries), which
	// goa's own encoder also supports; dropping it would be a regression against the stock one.
	e.req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return nil
}

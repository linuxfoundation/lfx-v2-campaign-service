// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"net/http"

	signedassetsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_signed_assets/server"
	signedassetsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_signed_assets"

	goahttp "goa.design/goa/v3/http"
)

// mountSignedAssets mounts the unauthenticated signed creative-asset route (LFXV2-2665) on mux.
//
// It is mounted separately from buildMux, and with its own encoder and formatter, because its
// error contract is the opposite of every other route's. Elsewhere a 400 names what was wrong
// so the caller can fix it; here the caller is anyone on the internet, and the route's whole
// security rests on giving them nothing to steer by. So:
//
//   - every request-decoding failure (a malformed id, a missing or non-numeric exp, a
//     signature of the wrong shape) and every rejection the service returns
//     (service.ErrSignedAssetNotFound: bad signature, expired, missing/foreign asset, feature
//     off) reaches the generated encoder's DEFAULT arm, which hands it to signedAssetFormatter
//     — a bare 404 for all of them;
//   - the encoder writes no body, so not even the validation message Goa would build can
//     escape, and the 404s are byte-identical whatever the cause.
//
// The one non-404 is the named 503 the service returns AFTER a signature has verified (the
// asset store not yet bound, or the asset-memory budget exhausted), which tells a holder of a
// valid URL nothing they do not already know.
func mountSignedAssets(mux goahttp.Muxer, svc signedassetsvc.Service, errhandler func(context.Context, http.ResponseWriter, error)) error {
	if svc == nil {
		return fmt.Errorf("mountSignedAssets: service is nil (the signed creative-asset route would be unmounted)")
	}
	server := signedassetsvr.New(signedassetsvc.NewEndpoints(svc), mux, goahttp.RequestDecoder, discardEncoder, errhandler, signedAssetFormatter)
	signedassetsvr.Mount(mux, server)
	return nil
}

// signedAssetFormatter maps every unnamed error on the signed-asset route to the same 404.
func signedAssetFormatter(context.Context, error) goahttp.Statuser { return signedAssetNotFound{} }

type signedAssetNotFound struct{}

func (signedAssetNotFound) StatusCode() int { return http.StatusNotFound }

// discardEncoder writes nothing: the signed-asset route's success body is the raw image
// (SkipResponseBodyEncodeDecode never calls an encoder), and its error responses are bodyless.
func discardEncoder(context.Context, http.ResponseWriter) goahttp.Encoder { return nopEncoder{} }

type nopEncoder struct{}

func (nopEncoder) Encode(any) error { return nil }

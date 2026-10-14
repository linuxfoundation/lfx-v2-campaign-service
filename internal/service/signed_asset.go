// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	signedassetsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_signed_assets"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/log"
)

// ErrSignedAssetNotFound is the ONE error every rejected signed-asset request resolves to:
// the feature off, a bad or expired signature, a missing, foreign or archived asset, a
// disallowed stored type. It is deliberately NOT a Goa named error, so it reaches the
// generated encoder's default arm and is rendered by the mount's formatter exactly as a
// request-decoding failure is — the same bare 404, with the same headers. A named error would
// add a `goa-error` header that decode failures do not carry, which is a difference a caller
// could tell apart.
var ErrSignedAssetNotFound = errors.New("signed creative asset not found")

// signedAssetReader is the read-only slice of the creative-asset store the serve path needs:
// price the asset, then load it. Same least-privilege shape as the Meta dispatcher's reader.
type signedAssetReader interface {
	GetAsset(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error)
	GetAssetSize(ctx context.Context, projectID, briefID, assetID string) (int64, error)
}

// signedAssetBudget is the process-wide asset-memory budget (dispatch.AssetReserver). A nil
// value reserves nothing.
type signedAssetBudget interface {
	Reserve(ctx context.Context, want int64) (func(), bool)
}

// signedAssetCacheControl is no-store, not `private, max-age=<remaining>`. The URL has exactly
// one intended reader — Reddit's fetcher, once, during the post create — so caching buys
// nothing, while ANY cache (the gateway, a CDN placed in front later, the fetcher's own) is a
// copy that can outlive the signature. no-store is the only value that guarantees the bytes are
// not served by anyone after the URL has expired.
const signedAssetCacheControl = "no-store"

// SignedAssetService serves a stored creative asset at a signed URL (LFXV2-2665). It is
// constructed on every container path so the route is always mounted; with no signer or no
// repository bound it answers the generic 404 (feature off) or 503 (verified, store not yet
// bound), never a bare unmounted-route 404 that would differ in body.
type SignedAssetService struct {
	signer *assetsign.Signer
	budget signedAssetBudget

	mu   sync.RWMutex
	repo signedAssetReader
}

var _ signedassetsvc.Service = (*SignedAssetService)(nil)

// NewSignedAssetService builds the service. signer nil means the feature is OFF.
func NewSignedAssetService(signer *assetsign.Signer, budget signedAssetBudget) *SignedAssetService {
	return &SignedAssetService{signer: signer, budget: budget}
}

// SetCreativeAssetRepo late-binds the store, on the fast path and on the cold-start retry.
// Guard the concrete pointer at the call site: a typed nil would read as bound.
func (s *SignedAssetService) SetCreativeAssetRepo(r signedAssetReader) {
	s.mu.Lock()
	s.repo = r
	s.mu.Unlock()
}

func (s *SignedAssetService) reader() signedAssetReader {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repo
}

// GetSignedCreativeAsset verifies the signature, re-checks that the asset still exists under
// that brief and project (and that the brief is still active — GetAsset's scope), and serves
// the stored bytes with the verified stored MIME type.
//
// Order is the security property: the HMAC is checked BEFORE any database access, so an
// unsigned request costs one MAC and nothing else, and no response can reveal whether an
// asset exists to someone who does not hold a valid URL for it. Logs carry the asset id and
// expiry only — never the signature or the URL.
func (s *SignedAssetService) GetSignedCreativeAsset(ctx context.Context, p *signedassetsvc.GetSignedCreativeAssetPayload) (*signedassetsvc.SignedCreativeAssetResult, io.ReadCloser, error) {
	if p == nil || !s.signer.Verify(p.ProjectID, p.BriefID, p.AssetID, p.Exp, p.Sig) {
		return nil, nil, ErrSignedAssetNotFound
	}
	logger := slog.With("asset_id", p.AssetID, "exp", p.Exp)

	repo := s.reader()
	if repo == nil {
		return nil, nil, unavailable()
	}
	size, err := repo.GetAssetSize(ctx, p.ProjectID, p.BriefID, p.AssetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			logger.InfoContext(ctx, "signed creative asset no longer exists for its brief")
			return nil, nil, ErrSignedAssetNotFound
		}
		logger.ErrorContext(ctx, "signed creative asset: size lookup failed", log.ErrKey, err)
		return nil, nil, unavailable()
	}
	if size <= 0 || size > maxCreativeStoredBytes {
		return nil, nil, ErrSignedAssetNotFound
	}

	// Priced BEFORE the load, from the same process-wide budget the Meta and Microsoft dispatches charge, so
	// the bytes this request materialises are inside the bound rather than beside it.
	release := func() {}
	if s.budget != nil {
		rel, ok := s.budget.Reserve(ctx, size)
		if !ok {
			logger.WarnContext(ctx, "signed creative asset: asset memory budget exhausted")
			return nil, nil, unavailable()
		}
		release = rel
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()

	asset, err := repo.GetAsset(ctx, p.ProjectID, p.BriefID, p.AssetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, ErrSignedAssetNotFound
		}
		logger.ErrorContext(ctx, "signed creative asset: load failed", log.ErrKey, err)
		return nil, nil, unavailable()
	}
	if len(asset.Bytes) == 0 || (asset.MimeType != model.MimeTypePNG && asset.MimeType != model.MimeTypeJPEG) {
		return nil, nil, ErrSignedAssetNotFound
	}

	logger.InfoContext(ctx, "serving signed creative asset", "bytes", len(asset.Bytes))
	body := &releasingReader{Reader: bytes.NewReader(asset.Bytes), release: release}
	handedOff = true
	return &signedassetsvc.SignedCreativeAssetResult{
		ContentType:         asset.MimeType,
		ContentLength:       int64(len(asset.Bytes)),
		CacheControl:        signedAssetCacheControl,
		ContentDisposition:  "inline",
		XContentTypeOptions: "nosniff",
	}, body, nil
}

func unavailable() error {
	return &signedassetsvc.SignedAssetUnavailableError{Code: "503", Message: "The service is unavailable."}
}

// releasingReader returns the asset-memory reservation when the generated handler closes the
// body (it always defers Close), i.e. once the bytes are written or the write failed. Embedding
// *bytes.Reader keeps io.WriterTo, which the generated handler prefers.
type releasingReader struct {
	*bytes.Reader
	release func()
	once    sync.Once
}

func (r *releasingReader) Close() error {
	r.once.Do(r.release)
	return nil
}
